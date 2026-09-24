package cache

// fileRefKey identifies a file across hardlinks (same volume + file index / inode).
type fileRefKey string
