package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"

	storageheader "github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const markerPrefix = ".template-gc/candidates/"

type limits struct {
	maxObjects          int
	maxBuilds           int
	maxHeaderBytes      int64
	maxObjectsPerDelete int
	maxMarkerWrites     int
	maxBuildDeletes     int
}

type buildState struct {
	newestUpdated time.Time
	objectCount   int
}

type markerState struct {
	created    time.Time
	generation int64
}

type inventory struct {
	builds  map[string]buildState
	markers map[string]markerState
	objects int
}

type actionKind string

const (
	actionMark   actionKind = "mark"
	actionReset  actionKind = "reset"
	actionDelete actionKind = "delete"
	actionUnmark actionKind = "unmark"
)

type action struct {
	buildID string
	kind    actionKind
}

type plan struct {
	reachable int
	actions   []action
}

type markerDocument struct {
	Version             int       `json:"version"`
	BuildID             string    `json:"build_id"`
	FirstUnreferencedAt time.Time `json:"first_unreferenced_at"`
}

func readRoots(path string, maxBuilds int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open roots file: %w", err)
	}
	defer file.Close()

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		id, parseErr := uuid.Parse(value)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid root build ID %q: %w", value, parseErr)
		}
		seen[id.String()] = struct{}{}
		if len(seen) > maxBuilds {
			return nil, fmt.Errorf("root count exceeded limit %d", maxBuilds)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read roots file: %w", err)
	}

	roots := make([]string, 0, len(seen))
	for id := range seen {
		roots = append(roots, id)
	}
	sort.Strings(roots)
	return roots, nil
}

func scanInventory(ctx context.Context, bucket *gcs.BucketHandle, bounded limits) (inventory, error) {
	result := inventory{
		builds:  make(map[string]buildState),
		markers: make(map[string]markerState),
	}
	objects := bucket.Objects(ctx, nil)
	for {
		attrs, err := objects.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return inventory{}, fmt.Errorf("list template objects: %w", err)
		}
		result.objects++
		if result.objects > bounded.maxObjects {
			return inventory{}, fmt.Errorf("object count exceeded limit %d", bounded.maxObjects)
		}

		if strings.HasPrefix(attrs.Name, markerPrefix) {
			id, ok := markerBuildID(attrs.Name)
			if !ok {
				continue
			}
			created := attrs.Created
			if created.IsZero() {
				created = attrs.Updated
			}
			result.markers[id] = markerState{created: created, generation: attrs.Generation}
			continue
		}

		id, ok := objectBuildID(attrs.Name)
		if !ok {
			continue
		}
		state := result.builds[id]
		state.objectCount++
		if attrs.Updated.After(state.newestUpdated) {
			state.newestUpdated = attrs.Updated
		}
		result.builds[id] = state
		if len(result.builds) > bounded.maxBuilds {
			return inventory{}, fmt.Errorf("build count exceeded limit %d", bounded.maxBuilds)
		}
	}
	return result, nil
}

func markerBuildID(name string) (string, bool) {
	value := strings.TrimSuffix(strings.TrimPrefix(name, markerPrefix), ".json")
	if value == name || strings.Contains(value, "/") {
		return "", false
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

func objectBuildID(name string) (string, bool) {
	prefix, _, ok := strings.Cut(name, "/")
	if !ok {
		return "", false
	}
	id, err := uuid.Parse(prefix)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

func computeReachable(
	ctx context.Context,
	roots []string,
	maxBuilds int,
	loadEdges func(context.Context, string) ([]string, error),
) (map[string]struct{}, error) {
	reachable := make(map[string]struct{}, len(roots))
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[0]
		queue = queue[1:]
		if _, ok := reachable[id]; ok {
			continue
		}
		reachable[id] = struct{}{}
		if len(reachable) > maxBuilds {
			return nil, fmt.Errorf("reachable build count exceeded limit %d", maxBuilds)
		}

		edges, err := loadEdges(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load dependency edges for %s: %w", id, err)
		}
		for _, edge := range edges {
			if edge == id {
				continue
			}
			if _, ok := reachable[edge]; !ok {
				queue = append(queue, edge)
			}
		}
	}
	return reachable, nil
}

func loadHeaderEdges(
	ctx context.Context,
	bucket *gcs.BucketHandle,
	knownBuilds map[string]buildState,
	buildID string,
	maxHeaderBytes int64,
) ([]string, error) {
	if _, exists := knownBuilds[buildID]; !exists {
		return nil, nil
	}

	edges := make(map[string]struct{})
	foundHeader := false
	for _, suffix := range []string{"memfile.header", "rootfs.ext4.header"} {
		name := buildID + "/" + suffix
		object := bucket.Object(name)
		attrs, err := object.Attrs(ctx)
		if errors.Is(err, gcs.ErrObjectNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", name, err)
		}
		if attrs.Size > maxHeaderBytes {
			return nil, fmt.Errorf("header %s size %d exceeded limit %d", name, attrs.Size, maxHeaderBytes)
		}

		reader, err := object.NewReader(ctx)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxHeaderBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", name, closeErr)
		}
		if int64(len(data)) > maxHeaderBytes {
			return nil, fmt.Errorf("header %s exceeded limit %d", name, maxHeaderBytes)
		}
		header, err := storageheader.DeserializeBytes(data)
		if err != nil {
			return nil, fmt.Errorf("deserialize %s: %w", name, err)
		}
		foundHeader = true
		for id := range header.Builds {
			if id != uuid.Nil {
				edges[id.String()] = struct{}{}
			}
		}
		for _, mapping := range header.Mapping.All() {
			if mapping.BuildId != uuid.Nil {
				edges[mapping.BuildId.String()] = struct{}{}
			}
		}
	}
	if !foundHeader {
		return nil, errors.New("build has stored objects but no readable memfile or rootfs header")
	}

	result := make([]string, 0, len(edges))
	for id := range edges {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func makePlan(now time.Time, retention time.Duration, unreferencedAge time.Duration, scanned inventory, reachable map[string]struct{}) plan {
	actions := make([]action, 0)
	for id, marker := range scanned.markers {
		if _, ok := reachable[id]; ok {
			actions = append(actions, action{buildID: id, kind: actionUnmark})
			continue
		}
		build, exists := scanned.builds[id]
		if !exists {
			actions = append(actions, action{buildID: id, kind: actionUnmark})
			continue
		}
		if build.newestUpdated.After(marker.created) {
			actions = append(actions, action{buildID: id, kind: actionReset})
			continue
		}
		if now.Sub(build.newestUpdated) < retention {
			continue
		}
		if now.Sub(marker.created) >= unreferencedAge {
			actions = append(actions, action{buildID: id, kind: actionDelete})
		}
	}
	for id := range scanned.builds {
		if _, ok := reachable[id]; ok {
			continue
		}
		if _, ok := scanned.markers[id]; !ok {
			actions = append(actions, action{buildID: id, kind: actionMark})
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].kind == actions[j].kind {
			return actions[i].buildID < actions[j].buildID
		}
		return actions[i].kind < actions[j].kind
	})
	return plan{reachable: len(reachable), actions: actions}
}

func reachabilityCoverage(roots []string, builds map[string]buildState, reachable map[string]struct{}) (storedRoots int, storedReachable int, ratio float64) {
	for _, id := range roots {
		if _, ok := builds[id]; ok {
			storedRoots++
		}
	}
	for id := range reachable {
		if _, ok := builds[id]; ok {
			storedReachable++
		}
	}
	if len(builds) > 0 {
		ratio = float64(storedReachable) / float64(len(builds))
	}
	return storedRoots, storedReachable, ratio
}

func validateApplySafety(
	roots []string,
	builds map[string]buildState,
	reachable map[string]struct{},
	minRoots int,
	minReachableRatio float64,
) error {
	if len(roots) < minRoots {
		return fmt.Errorf("root manifest has %d builds, below minimum %d", len(roots), minRoots)
	}
	storedRoots, storedReachable, ratio := reachabilityCoverage(roots, builds, reachable)
	if storedRoots < minRoots {
		return fmt.Errorf("only %d roots have stored objects, below minimum %d", storedRoots, minRoots)
	}
	if ratio < minReachableRatio {
		return fmt.Errorf(
			"only %d of %d stored builds are reachable (%.4f), below minimum ratio %.4f",
			storedReachable,
			len(builds),
			ratio,
			minReachableRatio,
		)
	}
	return nil
}

func applyPlan(
	ctx context.Context,
	bucket *gcs.BucketHandle,
	now time.Time,
	scanStartedAt time.Time,
	scanned inventory,
	planned plan,
	bounded limits,
) (map[actionKind]int, error) {
	applied := make(map[actionKind]int)
	for _, item := range planned.actions {
		switch item.kind {
		case actionDelete:
			if applied[actionDelete] >= bounded.maxBuildDeletes {
				continue
			}
			changed, err := deleteBuild(ctx, bucket, item.buildID, scanStartedAt, bounded.maxObjectsPerDelete)
			if err != nil {
				return nil, err
			}
			if changed {
				if err := replaceMarker(ctx, bucket, item.buildID, scanned.markers[item.buildID], now); err != nil {
					return nil, err
				}
				applied[actionReset]++
				continue
			}
			if err := deleteMarker(ctx, bucket, item.buildID, scanned.markers[item.buildID]); err != nil {
				return nil, err
			}
			applied[actionDelete]++
		case actionMark:
			if applied[actionMark]+applied[actionReset] >= bounded.maxMarkerWrites {
				continue
			}
			if err := createMarker(ctx, bucket, item.buildID, now); err != nil {
				return nil, err
			}
			applied[actionMark]++
		case actionReset:
			if applied[actionMark]+applied[actionReset] >= bounded.maxMarkerWrites {
				continue
			}
			if err := replaceMarker(ctx, bucket, item.buildID, scanned.markers[item.buildID], now); err != nil {
				return nil, err
			}
			applied[actionReset]++
		case actionUnmark:
			if err := deleteMarker(ctx, bucket, item.buildID, scanned.markers[item.buildID]); err != nil {
				return nil, err
			}
			applied[actionUnmark]++
		default:
			return nil, fmt.Errorf("unknown action %q", item.kind)
		}
	}
	return applied, nil
}

func deleteBuild(ctx context.Context, bucket *gcs.BucketHandle, buildID string, scanStartedAt time.Time, maxObjects int) (bool, error) {
	objects := bucket.Objects(ctx, &gcs.Query{Prefix: buildID + "/"})
	attrs := make([]*gcs.ObjectAttrs, 0, 8)
	for {
		object, err := objects.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("list candidate %s: %w", buildID, err)
		}
		if object.Updated.After(scanStartedAt) {
			return true, nil
		}
		attrs = append(attrs, object)
		if len(attrs) > maxObjects {
			return false, fmt.Errorf("candidate %s object count exceeded limit %d", buildID, maxObjects)
		}
	}
	for _, object := range attrs {
		err := bucket.Object(object.Name).If(gcs.Conditions{GenerationMatch: object.Generation}).Delete(ctx)
		if errors.Is(err, gcs.ErrObjectNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("delete %s generation %d: %w", object.Name, object.Generation, err)
		}
	}
	return false, nil
}

func createMarker(ctx context.Context, bucket *gcs.BucketHandle, buildID string, now time.Time) error {
	document := markerDocument{Version: 1, BuildID: buildID, FirstUnreferencedAt: now.UTC()}
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	writer := bucket.Object(markerName(buildID)).If(gcs.Conditions{DoesNotExist: true}).NewWriter(ctx)
	writer.ContentType = "application/json"
	if _, err := io.Copy(writer, bytes.NewReader(data)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write marker for %s: %w", buildID, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close marker for %s: %w", buildID, err)
	}
	return nil
}

func replaceMarker(ctx context.Context, bucket *gcs.BucketHandle, buildID string, marker markerState, now time.Time) error {
	if err := deleteMarker(ctx, bucket, buildID, marker); err != nil {
		return err
	}
	return createMarker(ctx, bucket, buildID, now)
}

func deleteMarker(ctx context.Context, bucket *gcs.BucketHandle, buildID string, marker markerState) error {
	object := bucket.Object(markerName(buildID))
	if marker.generation != 0 {
		object = object.If(gcs.Conditions{GenerationMatch: marker.generation})
	}
	err := object.Delete(ctx)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete marker for %s: %w", buildID, err)
	}
	return nil
}

func markerName(buildID string) string {
	return markerPrefix + buildID + ".json"
}
