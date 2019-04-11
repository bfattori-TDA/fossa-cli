
package haskell

import (
	"github.com/fossas/fossa-cli/errors"
	"github.com/fossas/fossa-cli/exec"
	"github.com/fossas/fossa-cli/files"
	"github.com/fossas/fossa-cli/graph"
	"github.com/fossas/fossa-cli/module"
	"github.com/fossas/fossa-cli/pkg"
	"github.com/mitchellh/mapstructure"
	"path/filepath"
	"strings"
)

type Options struct {
	// TODO: strategy as enum?
	Strategy string `mapstructure:"strategy"`
}

type Analyzer struct {
	Module  module.Module
	Options Options
}

func New(m module.Module) (*Analyzer, error) {
	var options Options
	err := mapstructure.Decode(m.Options, &options)

	if err != nil {
		return nil, err
	}

	return &Analyzer{
		Module:  m,
		Options: options,
	}, nil
}

func (a *Analyzer) Analyze() (graph.Deps, error) {
	if a.Options.Strategy == "cabal-install" {
		return a.AnalyzeCabal()
	} else if a.Options.Strategy == "stack" {
		return a.AnalyzeStack()
	} else {
		panic("Unknown haskell analysis strategy: " + a.Options.Strategy)
	}
}

const CabalPlanRelPath = "dist-newstyle/cache/plan.json"

type CabalPlan struct {
	Packages []Package `mapstructure:"install-plan"`
}

type Package struct {
	Type    string   `mapstructure:"type"`
	Id      string   `mapstructure:"id"`
	Name    string   `mapstructure:"pkg-name"`
	Version string   `mapstructure:"pkg-version"`

	Components map[string]Component `mapstructure:"components"` // Not always present
	Depends    []string             `mapstructure:"depends"`    // Dependencies can be defined here or in Components.*.Depends
	Style      string               `mapstructure:"style"`      // Only exists for packages with type `configured`
}

type Component struct {
	Depends []string `mapstructure:"depends"`
}

func (a *Analyzer) AnalyzeCabal() (graph.Deps, error) {
	cabalPlanPath := filepath.Join(a.Module.Dir, CabalPlanRelPath)

	// If plan.json doesn't exist, generate it
	if exists, _ := files.Exists(cabalPlanPath); !exists {
		_, _, err := exec.Run(exec.Cmd{
			Name: "cabal",
			Argv: []string{"new-build", "--dry-run"},
		})

		if err != nil {
			return graph.Deps{}, err
		}
	}

	if exists, _ := files.Exists(cabalPlanPath); !exists {
		// TODO: fallback to another strategy?
		return graph.Deps{}, errors.New("Couldn't find or generate cabal solver plan")
	}

	// Parse cabal new-build's build plan
	var rawPlan map[string]interface{}
	var plan    CabalPlan

	if err := files.ReadJSON(&rawPlan, cabalPlanPath); err != nil {
		return graph.Deps{}, err
	}
	if err := mapstructure.Decode(rawPlan, &plan); err != nil {
		return graph.Deps{}, err
	}

	// Elements in this map are packages that haven't been scanned for
	// dependencies yet
	var packages = make(map[string]Package)
	for _, p := range plan.Packages {
		packages[p.Id] = p
	}

	// Determine which of the projects in the plan are ours. Our projects
	// will have type "configured" and style "local"
	var ourProjects []Package

	for id, p := range packages {
		if p.Type == "configured" && p.Style == "local" {
			ourProjects = append(ourProjects, p)
			delete(packages, id)
		}
	}

	// Determine direct imports
	var imports []pkg.Import

	for _, project := range ourProjects {
		// todo: duplicate code
		for _, depId := range project.Depends {
			if dep, ok := packages[depId]; ok {
				delete(packages, depId)

				imports = append(imports, pkg.Import{
					// TODO: do we need to include Target?
					Resolved: pkg.ID{
						Type:     pkg.Haskell,
						Name:     dep.Name,
						Revision: dep.Version,
					},
				})
			}
		}
		for _, component := range project.Components {
			// todo: duplicate code
			for _, depId := range component.Depends {
				if dep, ok := packages[depId]; ok {
					delete(packages, depId)

					imports = append(imports, pkg.Import{
						// TODO: do we need to include Target?
						Resolved: pkg.ID{
							Type:     pkg.Haskell,
							Name:     dep.Name,
							Revision: dep.Version,
						},
					})
				}
			}
		}
	}

	// Add remaining packages as transitive deps

	var transitive = make(map[pkg.ID]pkg.Package)

	for _, project := range packages {
		pkgId := pkg.ID{
			Type: pkg.Haskell,
			Name: project.Name,
			Revision: project.Version,
		}
		transitive[pkgId] = pkg.Package{
			ID: pkgId,
		}
	}

	deps := graph.Deps{
		Direct: imports,
		Transitive: transitive,
	}

	return deps, nil
}

func (a *Analyzer) AnalyzeStack() (graph.Deps, error) {
	// Stack ls dependencies outputs deps in the form:
	// packageone 0.0.1.0
	// packagetwo 0.0.1.0
	// ...

	localDepsStdout, _, err := exec.Run(exec.Cmd{
		Name: "stack",
		Argv: []string{"ls", "dependencies", "--depth", "1"},
	})

	if err != nil {
		return graph.Deps{}, err
	}

	allDepsStdout, _, err := exec.Run(exec.Cmd{
		Name: "stack",
		Argv: []string{"ls", "dependencies"},
	})

	// Keep track of recorded packages so we don't include them twice
	var seen = make(map[string]bool)

	// Our direct dependencies
	var imports []pkg.Import

	FoldStackDeps(seen, localDepsStdout, func(name string, version string) {
		imports = append(imports, pkg.Import{
			// TODO: do we need to include Target?
			Resolved: pkg.ID{
				Type:     pkg.Haskell,
				Name:     name,
				Revision: version,
			},
		})
	})

	// Our transitive dependencies
	var transitive = make(map[pkg.ID]pkg.Package)

	FoldStackDeps(seen, allDepsStdout, func(name string, version string){
		pkgId := pkg.ID{
			Type: pkg.Haskell,
			Name: name,
			Revision: version,
		}

		transitive[pkgId] = pkg.Package{
			ID: pkgId,
		}
	})

	deps := graph.Deps{
		Direct: imports,
		Transitive: transitive,
	}

	return deps, nil
}

func FoldStackDeps(seen map[string]bool, depsOutput string, consume func(string, string)) {
	for _, line := range strings.Split(depsOutput, "\n") {
		var dep = strings.Split(line, " ")

		if len(dep) < 2 {
			continue
		}

		var name    = dep[0]
		var version = dep[1]

		// Add to imports if we haven't seen this dep already
		if _, ok := seen[line]; !ok {
			seen[line] = true

			consume(name,version)
		}
	}
}

func (Analyzer) Clean() error {
	return nil
}

func (Analyzer) Build() error {
	return nil
}

func (Analyzer) IsBuilt() (bool, error) {
	return true, nil
}