package extractor

// IngestProgressFunc reports file-level ingest progress (processed/total files).
type IngestProgressFunc func(processed, total int64)
