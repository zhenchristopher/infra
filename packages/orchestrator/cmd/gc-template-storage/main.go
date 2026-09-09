package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	gcs "cloud.google.com/go/storage"
)

const (
	minimumRetention       = 7 * 24 * time.Hour
	minimumUnreferencedAge = 24 * time.Hour
)

func main() {
	var (
		bucketName        = flag.String("bucket", "", "GCS template bucket name")
		rootsPath         = flag.String("roots", "", "newline-delimited live build IDs")
		apply             = flag.Bool("apply", false, "write markers and delete eligible candidates")
		retention         = flag.Duration("retention", minimumRetention, "minimum age of an unprotected build prefix before deletion")
		unreferencedAge   = flag.Duration("unreferenced-age", minimumUnreferencedAge, "minimum time a build must remain unreferenced before deletion")
		timeout           = flag.Duration("timeout", 20*time.Minute, "overall command timeout")
		maxObjects        = flag.Int("max-objects", 5_000_000, "maximum bucket objects scanned")
		maxBuilds         = flag.Int("max-builds", 100_000, "maximum build prefixes or graph nodes")
		maxHeaderBytes    = flag.Int64("max-header-bytes", 64<<20, "maximum bytes read per header")
		maxDeleteObjects  = flag.Int("max-delete-objects", 1_000, "maximum objects in one deleted build prefix")
		maxMarkerWrites   = flag.Int("max-marker-writes", 1_000, "maximum marker writes per run")
		maxBuildDeletes   = flag.Int("max-build-deletes", 25, "maximum build prefixes deleted per run")
		minRoots          = flag.Int("min-roots", 10, "minimum live roots required before applying changes")
		minReachableRatio = flag.Float64("min-reachable-ratio", 0.05, "minimum stored-build fraction that must remain reachable")
	)
	flag.Parse()

	if *bucketName == "" || *rootsPath == "" {
		fatalf("--bucket and --roots are required")
	}
	if *retention < minimumRetention {
		fatalf("--retention must be at least %s", minimumRetention)
	}
	if *unreferencedAge < minimumUnreferencedAge {
		fatalf("--unreferenced-age must be at least %s", minimumUnreferencedAge)
	}
	bounded := limits{
		maxObjects:          *maxObjects,
		maxBuilds:           *maxBuilds,
		maxHeaderBytes:      *maxHeaderBytes,
		maxObjectsPerDelete: *maxDeleteObjects,
		maxMarkerWrites:     *maxMarkerWrites,
		maxBuildDeletes:     *maxBuildDeletes,
	}
	if bounded.maxObjects <= 0 || bounded.maxBuilds <= 0 || bounded.maxHeaderBytes <= 0 ||
		bounded.maxObjectsPerDelete <= 0 || bounded.maxMarkerWrites <= 0 || bounded.maxBuildDeletes <= 0 ||
		*minRoots <= 0 || *minReachableRatio <= 0 || *minReachableRatio > 1 {
		fatalf("all bounds must be positive and --min-reachable-ratio must not exceed 1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := gcs.NewClient(ctx)
	if err != nil {
		fatalf("create GCS client: %v", err)
	}
	defer client.Close()
	bucket := client.Bucket(*bucketName)

	roots, err := readRoots(*rootsPath, bounded.maxBuilds)
	if err != nil {
		fatalf("load roots: %v", err)
	}
	scanStartedAt := time.Now().UTC()
	scanned, err := scanInventory(ctx, bucket, bounded)
	if err != nil {
		fatalf("scan inventory: %v", err)
	}
	reachable, err := computeReachable(ctx, roots, bounded.maxBuilds, func(ctx context.Context, buildID string) ([]string, error) {
		return loadHeaderEdges(ctx, bucket, scanned.builds, buildID, bounded.maxHeaderBytes)
	})
	if err != nil {
		fatalf("mark dependency graph: %v", err)
	}
	storedRoots, storedReachable, reachableRatio := reachabilityCoverage(roots, scanned.builds, reachable)
	if *apply {
		if err := validateApplySafety(roots, scanned.builds, reachable, *minRoots, *minReachableRatio); err != nil {
			fatalf("refusing apply: %v", err)
		}
	}
	planned := makePlan(time.Now().UTC(), *retention, *unreferencedAge, scanned, reachable)

	result := map[string]any{
		"apply":                   *apply,
		"bucket":                  *bucketName,
		"objects_scanned":         scanned.objects,
		"builds_scanned":          len(scanned.builds),
		"markers_scanned":         len(scanned.markers),
		"database_roots":          len(roots),
		"reachable_builds":        planned.reachable,
		"stored_database_roots":   storedRoots,
		"stored_reachable_builds": storedReachable,
		"stored_reachable_ratio":  reachableRatio,
		"planned_actions":         summarizeActions(planned.actions),
		"retention_seconds":       int64(retention.Seconds()),
		"unreferenced_seconds":    int64(unreferencedAge.Seconds()),
		"scan_started_at":         scanStartedAt,
		"deletion_is_capped":      true,
	}
	if *apply {
		applied, err := applyPlan(ctx, bucket, time.Now().UTC(), scanStartedAt, scanned, planned, bounded)
		if err != nil {
			fatalf("apply plan: %v", err)
		}
		result["applied_actions"] = applied
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("encode result: %v", err)
	}
	fmt.Println(string(encoded))
}

func summarizeActions(actions []action) map[actionKind]int {
	result := make(map[actionKind]int)
	for _, item := range actions {
		result[item.kind]++
	}
	return result
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, "template storage GC: "+format+"\n", values...)
	os.Exit(1)
}
