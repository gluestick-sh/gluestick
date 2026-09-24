// Package catalog resolves package references, ensures the referenced buckets
// are present, and serves catalog browse and search queries against the
// in-memory manifest index.
package catalog

import (
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
)

// CatalogBucketSummary describes an indexed bucket in the software catalog.
type CatalogBucketSummary struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	PackageCount int    `json:"packageCount"`
}

// CatalogPackageQuery lists packages from the search index with optional bucket filter.
type CatalogPackageQuery struct {
	Bucket         string `json:"bucket"`
	Query          string `json:"query"`
	Page           int    `json:"page"`
	PageSize       int    `json:"pageSize"`
	HideDeprecated bool   `json:"hideDeprecated"`
}

// CatalogBucketsQuery filters bucket summaries for the software catalog.
type CatalogBucketsQuery struct {
	HideDeprecated bool `json:"hideDeprecated"`
}

// CatalogPackagePage is a paginated catalog listing result.
type CatalogPackagePage struct {
	Items    []*etypes.Package `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"pageSize"`
}

// CatalogResolveRequest resolves recipe / catalog package names against the search index.
type CatalogResolveRequest struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
}

// CatalogBuckets returns installed buckets with indexed package counts.
func CatalogBuckets(e *runtime.Engine, q CatalogBucketsQuery) []CatalogBucketSummary {
	if e.SearchIdx == nil {
		return nil
	}
	counts := e.SearchIdx.CountByBucket(q.HideDeprecated, hiddenCatalogPackages(e))
	buckets := e.BucketRegistry.List()
	out := make([]CatalogBucketSummary, 0, len(buckets))
	for _, b := range buckets {
		desc := bucket.GetKnownBucketDescription(b.Name)
		out = append(out, CatalogBucketSummary{
			Name:         b.Name,
			Description:  desc,
			PackageCount: counts[b.Name],
		})
	}
	return out
}

// ListCatalogPackages returns paginated packages from the search index.
func ListCatalogPackages(e *runtime.Engine, q CatalogPackageQuery) (*CatalogPackagePage, error) {
	if e.SearchIdx == nil {
		return &CatalogPackagePage{Items: []*etypes.Package{}}, nil
	}
	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 30
	}

	matches := e.SearchIdx.ListEntries(q.Bucket, q.Query, q.HideDeprecated, hiddenCatalogPackages(e))
	total := len(matches)
	offset := (page - 1) * pageSize
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}

	items := make([]*etypes.Package, 0, end-offset)
	for _, match := range matches[offset:end] {
		items = append(items, &etypes.Package{
			Name:        match.Name,
			Version:     match.Version,
			Description: match.Description,
			Bucket:      match.Bucket,
			Homepage:    match.Homepage,
			Deprecated:  match.BrowseDeprecated(),
		})
	}

	return &CatalogPackagePage{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

// ResolveCatalogPackages resolves package names for recipe cards.
func ResolveCatalogPackages(e *runtime.Engine, requests []CatalogResolveRequest) []*etypes.Package {
	if e.SearchIdx == nil || len(requests) == 0 {
		return nil
	}
	out := make([]*etypes.Package, 0, len(requests))
	for _, req := range requests {
		if req.Name == "" {
			continue
		}
		match := e.SearchIdx.ResolvePackage(req.Name, req.Bucket)
		if match == nil {
			continue
		}
		out = append(out, &etypes.Package{
			Name:        match.Name,
			Version:     match.Version,
			Description: match.Description,
			Bucket:      match.Bucket,
			Homepage:    match.Homepage,
			Deprecated:  match.BrowseDeprecated(),
		})
	}
	return out
}
