package main

import (
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// openCLIEngine creates the shared engine instance for CLI commands.
func openCLIEngine() (*engine.Engine, error) {
	return engine.NewEngine(&engine.EngineConfig{
		RootDir: glueRoot(),
		Verbose: verbose.Enabled(),
	})
}

// syncEngineBucketsAfterAdd indexes a newly added bucket and logs activity.
func syncEngineBucketsAfterAdd(eng *engine.Engine, bucketName string) {
	if eng == nil || bucketName == "" {
		return
	}
	eng.LoadSearchIndexBucket(bucketName)
	_ = eng.RecordBucketAddActivity(bucketName, "success", "")
}

// syncEngineBucketsAfterRemove drops bucket entries from the search index.
func syncEngineBucketsAfterRemove(eng *engine.Engine, bucketName string) {
	if eng == nil || bucketName == "" {
		return
	}
	eng.RemoveSearchIndexBucket(bucketName)
	_ = eng.RecordBucketRemoveActivity(bucketName, "success", "")
}

// syncEngineBucketsAfterUpdate rescans bucket manifests into the search index.
func syncEngineBucketsAfterUpdate(eng *engine.Engine, bucketNames ...string) {
	if eng == nil {
		return
	}
	if len(bucketNames) == 0 {
		eng.ReloadBuckets(true)
		_ = eng.RecordBucketUpdateActivity("*", "success", "")
		return
	}
	for _, name := range bucketNames {
		eng.LoadSearchIndexBucket(name)
	}
	label := bucketNames[0]
	if len(bucketNames) > 1 {
		label = label + ",..."
	}
	_ = eng.RecordBucketUpdateActivity(label, "success", "")
}
