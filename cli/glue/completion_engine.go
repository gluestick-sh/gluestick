package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gluestick-sh/core/engine"
)

func openEngineForCompletion() (*engine.Engine, error) {
	return engine.NewEngine(&engine.EngineConfig{
		RootDir: glueRoot(),
		Verbose: false,
	})
}

func catalogPackageRefs(eng *engine.Engine, prefix string, limit int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	packages, err := eng.Search(ctx, &engine.SearchRequest{
		Query: prefix,
		Limit: limit,
	}, engine.NewSilentReporter())
	if err != nil {
		return nil, err
	}

	refs := make([]string, 0, len(packages))
	seen := make(map[string]struct{}, len(packages))
	for _, pkg := range packages {
		ref := formatCatalogRef(pkg.Bucket, pkg.Name)
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func installedPackageNames(eng *engine.Engine, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	packages, err := eng.List(ctx, &engine.ListRequest{}, engine.NewSilentReporter())
	if err != nil {
		return nil, err
	}

	prefix = strings.ToLower(prefix)
	var names []string
	for _, pkg := range packages {
		if prefix != "" && !strings.HasPrefix(strings.ToLower(pkg.Name), prefix) {
			continue
		}
		names = append(names, pkg.Name)
	}
	sort.Strings(names)
	return names, nil
}

func formatCatalogRef(bucketName, pkgName string) string {
	if bucketName == "" || bucketName == "main" {
		return pkgName
	}
	return bucketName + "/" + pkgName
}
