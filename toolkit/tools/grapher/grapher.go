// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/exe"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/packagerepo/repocloner/rpmrepocloner"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkggraph"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/timestamp"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/profile"

	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	app    = kingpin.New("grapher", "Dependency graph generation tool")
	input  = exe.InputFlag(app, "Input json listing all local SRPMs")
	output = exe.OutputFlag(app, "Output file to export the graph to")

	logFile               = exe.LogFileFlag(app)
	logLevel              = exe.LogLevelFlag(app)
	profFlags             = exe.SetupProfileFlags(app)
	strictGoals           = app.Flag("strict-goals", "Don't allow missing goal packages").Bool()
	strictUnresolved      = app.Flag("strict-unresolved", "Don't allow missing unresolved packages").Bool()
	timestampFile         = app.Flag("timestamp-file", "File that stores timestamps for this program.").String()
	usePMCtoResolveCycles = app.Flag("usePMCtoresolvecycles", "Cycles will be resolved by downloading rpm packages from PMC if locally unavailable").Bool()
	tlsClientCert         = app.Flag("tls-cert", "TLS client certificate to use when downloading files.").String()
	tlsClientKey          = app.Flag("tls-key", "TLS client key to use when downloading files.").String()
	packageURLlist        = app.Flag("packageURLlist", "PACKAGE_URL_LIST").Strings()

	resolveCyclesFromUpstream     = app.Flag("resolve-cycles-from-upstream", "Let grapher resolve cycles by marking rpms available in repo as remote").Bool()
	outDir                        = exe.OutputDirFlag(app, "Directory to download packages into.")
	existingRpmsDir               = app.Flag("rpm-dir", "Directory that contains already built RPMs. Should contain top level directories for architecture.").Required().ExistingDir()
	existingToolchainRpmDir       = app.Flag("toolchain-rpms-dir", "Directory that contains already built toolchain RPMs. Should contain top level directories for architecture.").Required().ExistingDir()
	tmpDir                        = app.Flag("tmp-dir", "Directory to store temporary files while downloading.").String()
	workerTar                     = app.Flag("tdnf-worker", "Full path to worker_chroot.tar.gz").Required().ExistingFile()
	repoFiles                     = app.Flag("repo-file", "Full path to a repo file").Required().ExistingFiles()
	usePreviewRepo                = app.Flag("use-preview-repo", "Pull packages from the upstream preview repo").Bool()
	toolchainManifest             = app.Flag("toolchain-manifest", "Path to a list of RPMs which are created by the toolchain. Will mark RPMs from this list as prebuilt.").ExistingFile()
	ignoreVersionToResolveSelfDep = app.Flag("ignore-version-to-resolve-selfdep", "Ignore package version while downloading package from upstream when resolving cycle").Bool()

	depGraph = pkggraph.NewPkgGraph()
)

func main() {
	const goalNodeName = "ALL"

	app.Version(exe.ToolkitVersion)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger.InitBestEffort(*logFile, *logLevel)

	prof, err := profile.StartProfiling(profFlags)
	if err != nil {
		logger.Log.Warnf("Could not start profiling: %s", err)
	}
	defer prof.StopProfiler()

	timestamp.BeginTiming("grapher", *timestampFile)
	defer timestamp.CompleteTiming()

	localPackages := pkgjson.PackageRepo{}
	err = localPackages.ParsePackageJSON(*input)
	if err != nil {
		logger.Log.Panic(err)
	}

	depGraph := pkggraph.NewPkgGraph()
	err = populateGraph(depGraph, &localPackages)
	if err != nil {
		logger.Log.Panic(err)
	}

	// Add a default "ALL" goal to build everything local
	_, err = depGraph.AddGoalNode(goalNodeName, nil, *strictGoals)
	if err != nil {
		logger.Log.Panic(err)
	}

	var cloner *rpmrepocloner.RpmRepoCloner = nil
	/*
		disableUpstreamRepos is set to false because we want to download packages from upstream
		disableDefaultRepos is also set to false. This can be enabled by sending a flag to grapher
	*/
	if *resolveCyclesFromUpstream {
		cloner, err = rpmrepocloner.ConstructClonerWithNetwork(*outDir, *tmpDir, *workerTar, *existingRpmsDir, *existingToolchainRpmDir, *tlsClientCert, *tlsClientKey, *usePreviewRepo, false, false, *repoFiles)
		if err != nil {
			logger.Log.Panic(err)
		}
		defer cloner.Close()
	}
	logger.Log.Info("Running cycle resolution to fix any cycles in the dependency graph")
	err = depGraph.MakeDAG(*resolveCyclesFromUpstream, *ignoreVersionToResolveSelfDep, cloner)
	if err != nil {
		logger.Log.Panic(err)
	}

	err = pkggraph.WriteDOTGraphFile(depGraph, *output)
	if err != nil {
		logger.Log.Panic(err)
	}

	logger.Log.Info("Finished generating graph.")
}

// addUnresolvedPackage adds an unresolved node to the graph representing the
// packged described in the PackgetVer structure. Returns an error if the node
// could not be created.
func addUnresolvedPackage(g *pkggraph.PkgGraph, pkgVer *pkgjson.PackageVer) (newRunNode *pkggraph.PkgNode, err error) {
	logger.Log.Debugf("Adding unresolved %s", pkgVer)
	if *strictUnresolved {
		err = fmt.Errorf("strict-unresolved does not allow unresolved packages, attempting to add %s", pkgVer)
		return
	}

	nodes, err := g.FindBestPkgNode(pkgVer)
	if err != nil {
		return
	}
	if nodes != nil {
		err = fmt.Errorf(`attempted to mark a local package "%+v" as unresolved`, pkgVer)
		return
	}

	// Create a new node
	newRunNode, err = g.AddPkgNode(pkgVer, pkggraph.StateUnresolved, pkggraph.TypeRemoteRun, "<NO_SRPM_PATH>", "<NO_RPM_PATH>", "<NO_SPEC_PATH>", "<NO_SOURCE_PATH>", "<NO_ARCHITECTURE>", "<NO_REPO>")
	if err != nil {
		return
	}

	logger.Log.Infof("Adding unresolved node %s\n", newRunNode.FriendlyName())

	return
}

// addNodesForPackage creates a "Run" and "Build" node for the package described
// in the PackageVer structure. Returns pointers to the build and run Nodes
// created, or an error if one of the nodes could not be created.
func addNodesForPackage(g *pkggraph.PkgGraph, pkgVer *pkgjson.PackageVer, pkg *pkgjson.Package) (newRunNode *pkggraph.PkgNode, newBuildNode *pkggraph.PkgNode, err error) {
	nodes, err := g.FindExactPkgNodeFromPkg(pkgVer)
	if err != nil {
		return
	}
	if nodes != nil {
		logger.Log.Warnf(`Duplicate package name for package %+v read from SRPM "%s" (Previous: %+v)`, pkgVer, pkg.SrpmPath, nodes.RunNode)
		err = nil
		if nodes.RunNode != nil {
			newRunNode = nodes.RunNode
		}
		if nodes.BuildNode != nil {
			newBuildNode = nodes.BuildNode
		}
	}

	if newRunNode == nil {
		// Add "Run" node
		newRunNode, err = g.AddPkgNode(pkgVer, pkggraph.StateMeta, pkggraph.TypeLocalRun, pkg.SrpmPath, pkg.RpmPath, pkg.SpecPath, pkg.SourceDir, pkg.Architecture, "<LOCAL>")
		logger.Log.Debugf("Adding run node %s with id %d\n", newRunNode.FriendlyName(), newRunNode.ID())
		if err != nil {
			return
		}
	}

	if newBuildNode == nil {
		// Add "Build" node
		newBuildNode, err = g.AddPkgNode(pkgVer, pkggraph.StateBuild, pkggraph.TypeLocalBuild, pkg.SrpmPath, pkg.RpmPath, pkg.SpecPath, pkg.SourceDir, pkg.Architecture, "<LOCAL>")
		logger.Log.Debugf("Adding build node %s with id %d\n", newBuildNode.FriendlyName(), newBuildNode.ID())
		if err != nil {
			return
		}
	}

	// A "run" node has an implicit dependency on its coresponding "build" node, encode that here.
	err = g.AddEdge(newRunNode, newBuildNode)
	if err != nil {
		logger.Log.Errorf("Adding edge failed for %+v", pkgVer)
	}

	return
}

// addSingleDependency will add an edge between packageNode and the "Run" node for the
// dependency described in the PackageVer structure. Returns an error if the
// addition failed.
func addSingleDependency(g *pkggraph.PkgGraph, packageNode *pkggraph.PkgNode, dependency *pkgjson.PackageVer) (err error) {
	var dependentNode *pkggraph.PkgNode
	logger.Log.Tracef("Adding a dependency from %+v to %+v", packageNode.VersionedPkg, dependency)
	nodes, err := g.FindBestPkgNode(dependency)
	if err != nil {
		logger.Log.Errorf("Unable to check lookup list for %+v (%s)", dependency, err)
		return err
	}

	if nodes == nil {
		dependentNode, err = addUnresolvedPackage(g, dependency)
		if err != nil {
			logger.Log.Errorf(`Could not add a package "%s"`, dependency.Name)
			return err
		}
	} else {
		// All dependencies are assumed to be "Run" dependencies
		dependentNode = nodes.RunNode
	}

	if packageNode == dependentNode {
		logger.Log.Debugf("Package %+v requires itself!", packageNode)
		return nil
	}

	// Avoid creating runtime dependencies from an RPM to a different provide from the same RPM as the dependency will always be met on RPM installation.
	// Creating these edges may cause non-problematic cycles that can significantly increase memory usage and runtime during cycle resolution.
	// If there are enough of these cycles it can exhaust the system's memory when resolving them.
	// - Only check run nodes. If a build node has a reflexive cycle then it cannot be built without a bootstrap version.
	if packageNode.Type == pkggraph.TypeLocalRun &&
		dependentNode.Type == pkggraph.TypeLocalRun &&
		packageNode.RpmPath == dependentNode.RpmPath {

		logger.Log.Debugf("%+v requires %+v which is provided by the same RPM.", packageNode, dependentNode)
		return nil
	}

	err = g.AddEdge(packageNode, dependentNode)
	if err != nil {
		logger.Log.Errorf("Failed to add edge failed between %+v and %+v.", packageNode, dependency)
	}

	return err
}

// addLocalPackage adds the package provided by the Package structure, and
// updates the SRPM path name
func addLocalPackage(g *pkggraph.PkgGraph, pkg *pkgjson.Package) error {
	_, _, err := addNodesForPackage(g, pkg.Provides, pkg)
	return err
}

// addDependencies adds edges for both build and runtime requirements for the
// package described in the Package structure. Returns an error if the edges
// could not be created.
func addPkgDependencies(g *pkggraph.PkgGraph, pkg *pkgjson.Package) (dependenciesAdded int, err error) {
	provide := pkg.Provides
	runDependencies := pkg.Requires
	buildDependencies := pkg.BuildRequires

	// Find the current node in the lookup list.
	logger.Log.Debugf("Adding dependencies for package %s", pkg.SrpmPath)
	nodes, err := g.FindExactPkgNodeFromPkg(provide)
	if err != nil {
		return
	}
	if nodes == nil {
		return dependenciesAdded, fmt.Errorf("can't add dependencies to a missing package %+v", pkg)
	}
	runNode := nodes.RunNode
	buildNode := nodes.BuildNode

	// For each run time and build time dependency, add the edges
	logger.Log.Tracef("Adding run dependencies")
	for _, dependency := range runDependencies {
		err = addSingleDependency(g, runNode, dependency)
		if err != nil {
			logger.Log.Errorf("Unable to add run-time dependencies for %+v", pkg)
			return
		}
		dependenciesAdded++
	}

	logger.Log.Tracef("Adding build dependencies")
	for _, dependency := range buildDependencies {
		err = addSingleDependency(g, buildNode, dependency)
		if err != nil {
			logger.Log.Errorf("Unable to add build-time dependencies for %+v", pkg)
			return
		}
		dependenciesAdded++
	}

	return
}

// populateGraph adds all the data contained in the PackageRepo structure into
// the graph.
func populateGraph(graph *pkggraph.PkgGraph, repo *pkgjson.PackageRepo) (err error) {
	timestamp.StartEvent("populating graph", nil)
	defer timestamp.StopEvent(nil)

	packages := repo.Repo

	timestamp.StartEvent("add package node", nil)

	// Scan and add each package we know about
	logger.Log.Infof("Adding all packages from %s", *input)
	// NOTE: range iterates by value, not reference. Manually access slice
	for idx := range packages {
		pkg := packages[idx]
		err = addLocalPackage(graph, pkg)
		if err != nil {
			logger.Log.Errorf("Failed to add local package %+v", pkg)
			return err
		}
	}
	logger.Log.Infof("\tAdded %d packages", len(packages))

	timestamp.StopEvent(nil) // add package nodes
	timestamp.StartEvent("add dependencies", nil)

	// Rescan and add all the dependencies
	logger.Log.Infof("Adding all dependencies from %s", *input)
	dependenciesAdded := 0
	for idx := range packages {
		pkg := packages[idx]
		num, err := addPkgDependencies(graph, pkg)
		if err != nil {
			logger.Log.Errorf("Failed to add dependency %+v", pkg)
			return err
		}
		dependenciesAdded += num
	}
	logger.Log.Infof("\tAdded %d dependencies", dependenciesAdded)

	timestamp.StopEvent(nil) // add dependencies

	return err
}
