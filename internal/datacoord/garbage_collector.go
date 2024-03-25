// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datacoord

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/samber/lo"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/metastore/kv/binlog"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/metautil"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

// GcOption garbage collection options
type GcOption struct {
	cli              storage.ChunkManager // client
	enabled          bool                 // enable switch
	checkInterval    time.Duration        // each interval
	missingTolerance time.Duration        // key missing in meta tolerance time
	dropTolerance    time.Duration        // dropped segment related key tolerance time
	scanInterval     time.Duration        // interval for scan residue for interupted log wrttien

	removeLogPool *conc.Pool[struct{}]
}

// garbageCollector handles garbage files in object storage
// which could be dropped collection remanent or data node failure traces
type garbageCollector struct {
	option  GcOption
	meta    *meta
	handler Handler

	startOnce  sync.Once
	stopOnce   sync.Once
	wg         sync.WaitGroup
	closeCh    chan struct{}
	cmdCh      chan gcCmd
	pauseUntil atomic.Time
}
type gcCmd struct {
	cmdType  datapb.GcCommand
	duration time.Duration
	done     chan struct{}
}

// newGarbageCollector create garbage collector with meta and option
func newGarbageCollector(meta *meta, handler Handler, opt GcOption) *garbageCollector {
	log.Info("GC with option",
		zap.Bool("enabled", opt.enabled),
		zap.Duration("interval", opt.checkInterval),
		zap.Duration("scanInterval", opt.scanInterval),
		zap.Duration("missingTolerance", opt.missingTolerance),
		zap.Duration("dropTolerance", opt.dropTolerance))
	opt.removeLogPool = conc.NewPool[struct{}](Params.DataCoordCfg.GCRemoveConcurrent.GetAsInt(), conc.WithExpiryDuration(time.Minute))
	return &garbageCollector{
		meta:    meta,
		handler: handler,
		option:  opt,
		closeCh: make(chan struct{}),
		cmdCh:   make(chan gcCmd),
	}
}

// start a goroutine and perform gc check every `checkInterval`
func (gc *garbageCollector) start() {
	if gc.option.enabled {
		if gc.option.cli == nil {
			log.Warn("DataCoord gc enabled, but SSO client is not provided")
			return
		}
		gc.startOnce.Do(func() {
			gc.wg.Add(1)
			go gc.work()
		})
	}
}

func (gc *garbageCollector) Pause(ctx context.Context, pauseDuration time.Duration) error {
	if !gc.option.enabled {
		log.Info("garbage collection not enabled")
		return nil
	}
	done := make(chan struct{})
	select {
	case gc.cmdCh <- gcCmd{
		cmdType:  datapb.GcCommand_Pause,
		duration: pauseDuration,
		done:     done,
	}:
		<-done
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (gc *garbageCollector) Resume(ctx context.Context) error {
	if !gc.option.enabled {
		log.Warn("garbage collection not enabled, cannot resume")
		return merr.WrapErrServiceUnavailable("garbage collection not enabled")
	}
	done := make(chan struct{})
	select {
	case gc.cmdCh <- gcCmd{
		cmdType: datapb.GcCommand_Resume,
		done:    done,
	}:
		<-done
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// work contains actual looping check logic
func (gc *garbageCollector) work() {
	defer gc.wg.Done()
	ticker := time.NewTicker(gc.option.checkInterval)
	defer ticker.Stop()
	scanTicker := time.NewTicker(gc.option.scanInterval)
	defer scanTicker.Stop()
	for {
		select {
		case <-ticker.C:
			if time.Now().Before(gc.pauseUntil.Load()) {
				log.Info("garbage collector paused", zap.Time("until", gc.pauseUntil.Load()))
				continue
			}
			gc.clearEtcd()
			gc.recycleUnusedIndexes()
			gc.recycleUnusedSegIndexes()
			gc.recycleUnusedIndexFiles()
		case <-scanTicker.C:
			log.Info("Garbage collector start to scan interrupted write residue")
			gc.scan()
		case cmd := <-gc.cmdCh:
			switch cmd.cmdType {
			case datapb.GcCommand_Pause:
				pauseUntil := time.Now().Add(cmd.duration)
				if pauseUntil.After(gc.pauseUntil.Load()) {
					log.Info("garbage collection paused", zap.Duration("duration", cmd.duration), zap.Time("pauseUntil", pauseUntil))
					gc.pauseUntil.Store(pauseUntil)
				} else {
					log.Info("new pause until before current value", zap.Duration("duration", cmd.duration), zap.Time("pauseUntil", pauseUntil), zap.Time("oldPauseUntil", gc.pauseUntil.Load()))
				}
			case datapb.GcCommand_Resume:
				// reset to zero value
				gc.pauseUntil.Store(time.Time{})
				log.Info("garbage collection resumed")
			}
			close(cmd.done)
		case <-gc.closeCh:
			log.Warn("garbage collector quit")
			return
		}
	}
}

func (gc *garbageCollector) close() {
	gc.stopOnce.Do(func() {
		close(gc.closeCh)
		gc.wg.Wait()
	})
}

// scan load meta file info and compares OSS keys
// if missing found, performs gc cleanup
func (gc *garbageCollector) scan() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		total   = 0
		valid   = 0
		missing = 0
	)

	// walk only data cluster related prefixes
	prefixes := make([]string, 0, 3)
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), common.SegmentInsertLogPath))
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), common.SegmentStatslogPath))
	prefixes = append(prefixes, path.Join(gc.option.cli.RootPath(), common.SegmentDeltaLogPath))
	labels := []string{metrics.InsertFileLabel, metrics.StatFileLabel, metrics.DeleteFileLabel}
	var removedKeys []string

	checker := func(segmentID typeutil.UniqueID, objectPath, prefix string) bool {
		results := gc.meta.GetSegments([]typeutil.UniqueID{segmentID}, func(info *SegmentInfo) bool {
			if strings.Contains(prefix, common.SegmentInsertLogPath) {
				return true
			} else if strings.Contains(prefix, common.SegmentStatslogPath) {
				for _, fieldBinlog := range info.GetStatslogs() {
					for _, b := range fieldBinlog.GetBinlogs() {
						if b.GetLogPath() == objectPath {
							return true
						}
					}
				}
				return false
			} else if strings.Contains(prefix, common.SegmentDeltaLogPath) {
				for _, fieldBinlog := range info.GetDeltalogs() {
					for _, b := range fieldBinlog.GetBinlogs() {
						if b.GetLogPath() == objectPath {
							return true
						}
					}
				}
				return false
			}
			return true
		})
		return len(results) == 1
	}

	for idx, prefix := range prefixes {
		startTs := time.Now()
		objectPathHolderChan := gc.option.cli.ListWithPrefix(ctx, prefix, true)
		//segmentMap, filesMap := getMetaMap()
		for objectPathHolder := range objectPathHolderChan {
			if objectPathHolder.Err != nil {
				log.Error("failed to list object",
					zap.String("prefix", prefix),
					zap.Error(objectPathHolder.Err))
				continue
			}
			if time.Since(objectPathHolder.ModTime) <= gc.option.missingTolerance {
				continue
			}
			total++
			infoKey := objectPathHolder.Path

			segmentID, err := storage.ParseSegmentIDByBinlog(gc.option.cli.RootPath(), infoKey)
			if err != nil {
				missing++
				log.Warn("parse segment id error",
					zap.String("infoKey", infoKey),
					zap.Error(err))
				continue
			}
			if checker(segmentID, infoKey, prefix) {
				valid++
				continue
			}

			removedKeys = append(removedKeys, infoKey)
			err = gc.option.cli.Remove(ctx, infoKey)
			if err != nil {
				missing++
				log.Error("failed to remove object",
					zap.String("infoKey", infoKey),
					zap.Error(err))
			}
		}
		cost := time.Since(startTs)
		log.Info("gc scan finish one round", zap.String("prefix", prefix), zap.Duration("time spent", cost))
		metrics.GarbageCollectorListLatency.
			WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), labels[idx]).
			Observe(float64(cost.Milliseconds()))
	}
	metrics.GarbageCollectorRunCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Add(1)
	log.Info("scan file to do garbage collection",
		zap.Int("total", total),
		zap.Int("valid", valid),
		zap.Int("missing", missing),
		zap.Strings("removedKeys", removedKeys))
}

func (gc *garbageCollector) checkDroppedSegmentGC(segment *SegmentInfo,
	childSegment *SegmentInfo,
	indexSet typeutil.UniqueSet,
	cpTimestamp Timestamp,
) bool {
	log := log.With(zap.Int64("segmentID", segment.ID))

	isCompacted := childSegment != nil || segment.GetCompacted()
	if isCompacted {
		// For compact A, B -> C, don't GC A or B if C is not indexed,
		// guarantee replacing A, B with C won't downgrade performance
		// If the child is GC'ed first, then childSegment will be nil.
		if childSegment != nil && !indexSet.Contain(childSegment.GetID()) {
			log.WithRateGroup("GC_FAIL_COMPACT_TO_NOT_INDEXED", 1, 60).
				RatedInfo(60, "skipping GC when compact target segment is not indexed",
					zap.Int64("child segment ID", childSegment.GetID()))
			return false
		}
	} else {
		if !gc.isExpire(segment.GetDroppedAt()) {
			return false
		}
	}

	segInsertChannel := segment.GetInsertChannel()
	// Ignore segments from potentially dropped collection. Check if collection is to be dropped by checking if channel is dropped.
	// We do this because collection meta drop relies on all segment being GCed.
	if gc.meta.catalog.ChannelExists(context.Background(), segInsertChannel) &&
		segment.GetDmlPosition().GetTimestamp() > cpTimestamp {
		// segment gc shall only happen when channel cp is after segment dml cp.
		log.WithRateGroup("GC_FAIL_CP_BEFORE", 1, 60).
			RatedInfo(60, "dropped segment dml position after channel cp, skip meta gc",
				zap.Uint64("dmlPosTs", segment.GetDmlPosition().GetTimestamp()),
				zap.Uint64("channelCpTs", cpTimestamp),
			)
		return false
	}
	return true
}

func (gc *garbageCollector) clearEtcd() {
	all := gc.meta.SelectSegments(func(si *SegmentInfo) bool { return true })
	drops := make(map[int64]*SegmentInfo, 0)
	compactTo := make(map[int64]*SegmentInfo)
	channels := typeutil.NewSet[string]()
	for _, segment := range all {
		cloned := segment.Clone()
		binlog.DecompressBinLogs(cloned.SegmentInfo)
		if cloned.GetState() == commonpb.SegmentState_Dropped {
			drops[cloned.GetID()] = cloned
			channels.Insert(cloned.GetInsertChannel())
			// continue
			// A(indexed), B(indexed) -> C(no indexed), D(no indexed) -> E(no indexed), A, B can not be GC
		}
		for _, from := range cloned.GetCompactionFrom() {
			compactTo[from] = cloned
		}
	}

	droppedCompactTo := make(map[*SegmentInfo]struct{})
	for id := range drops {
		if to, ok := compactTo[id]; ok {
			droppedCompactTo[to] = struct{}{}
		}
	}
	indexedSegments := FilterInIndexedSegments(gc.handler, gc.meta, lo.Keys(droppedCompactTo)...)
	indexedSet := make(typeutil.UniqueSet)
	for _, segment := range indexedSegments {
		indexedSet.Insert(segment.GetID())
	}

	channelCPs := make(map[string]uint64)
	for channel := range channels {
		pos := gc.meta.GetChannelCheckpoint(channel)
		channelCPs[channel] = pos.GetTimestamp()
	}

	dropIDs := lo.Keys(drops)
	sort.Slice(dropIDs, func(i, j int) bool {
		return dropIDs[i] < dropIDs[j]
	})

	log.Info("start to GC segments", zap.Int("drop_num", len(dropIDs)))
	for _, segmentID := range dropIDs {
		segment, ok := drops[segmentID]
		if !ok {
			log.Warn("segmentID is not in drops", zap.Int64("segmentID", segmentID))
			continue
		}

		segInsertChannel := segment.GetInsertChannel()
		if !gc.checkDroppedSegmentGC(segment, compactTo[segment.GetID()], indexedSet, channelCPs[segInsertChannel]) {
			continue
		}

		logs := getLogs(segment)
		log.Info("GC segment", zap.Int64("segmentID", segment.GetID()),
			zap.Int("insert_logs", len(segment.GetBinlogs())),
			zap.Int("delta_logs", len(segment.GetDeltalogs())),
			zap.Int("stats_logs", len(segment.GetStatslogs())))
		if gc.removeLogs(logs) {
			err := gc.meta.DropSegment(segment.GetID())
			if err != nil {
				log.Info("GC segment meta failed to drop segment", zap.Int64("segment id", segment.GetID()), zap.Error(err))
			} else {
				log.Info("GC segment meta drop semgent", zap.Int64("segment id", segment.GetID()))
			}
		}
		if segList := gc.meta.GetSegmentsByChannel(segInsertChannel); len(segList) == 0 &&
			!gc.meta.catalog.ChannelExists(context.Background(), segInsertChannel) {
			log.Info("empty channel found during gc, manually cleanup channel checkpoints", zap.String("vChannel", segInsertChannel))
			if err := gc.meta.DropChannelCheckpoint(segInsertChannel); err != nil {
				log.Info("failed to drop channel check point during segment garbage collection", zap.String("vchannel", segInsertChannel), zap.Error(err))
			}
		}
	}
}

func (gc *garbageCollector) isExpire(dropts Timestamp) bool {
	droptime := time.Unix(0, int64(dropts))
	return time.Since(droptime) > gc.option.dropTolerance
}

func getLogs(sinfo *SegmentInfo) []*datapb.Binlog {
	var logs []*datapb.Binlog
	for _, flog := range sinfo.GetBinlogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}

	for _, flog := range sinfo.GetStatslogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}

	for _, flog := range sinfo.GetDeltalogs() {
		logs = append(logs, flog.GetBinlogs()...)
	}
	return logs
}

func (gc *garbageCollector) removeLogs(logs []*datapb.Binlog) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var w sync.WaitGroup
	w.Add(len(logs))
	for _, l := range logs {
		tmpLog := l
		gc.option.removeLogPool.Submit(func() (struct{}, error) {
			defer w.Done()
			select {
			case <-ctx.Done():
				return struct{}{}, nil
			default:
				err := gc.option.cli.Remove(ctx, tmpLog.GetLogPath())
				if err != nil {
					switch err.(type) {
					case minio.ErrorResponse:
						errResp := minio.ToErrorResponse(err)
						if errResp.Code != "" && errResp.Code != "NoSuchKey" {
							cancel()
						}
					default:
						cancel()
					}
				}
				return struct{}{}, nil
			}
		})
	}
	w.Wait()
	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

func (gc *garbageCollector) recycleUnusedIndexes() {
	log.Info("start recycleUnusedIndexes")
	deletedIndexes := gc.meta.indexMeta.GetDeletedIndexes()
	for _, index := range deletedIndexes {
		if err := gc.meta.indexMeta.RemoveIndex(index.CollectionID, index.IndexID); err != nil {
			log.Warn("remove index on collection fail", zap.Int64("collectionID", index.CollectionID),
				zap.Int64("indexID", index.IndexID), zap.Error(err))
			continue
		}
	}
}

func (gc *garbageCollector) recycleUnusedSegIndexes() {
	segIndexes := gc.meta.indexMeta.GetAllSegIndexes()
	for _, segIdx := range segIndexes {
		if gc.meta.GetSegment(segIdx.SegmentID) == nil || !gc.meta.indexMeta.IsIndexExist(segIdx.CollectionID, segIdx.IndexID) {
			if err := gc.meta.indexMeta.RemoveSegmentIndex(segIdx.CollectionID, segIdx.PartitionID, segIdx.SegmentID, segIdx.IndexID, segIdx.BuildID); err != nil {
				log.Warn("delete index meta from etcd failed, wait to retry", zap.Int64("buildID", segIdx.BuildID),
					zap.Int64("segmentID", segIdx.SegmentID), zap.Int64("nodeID", segIdx.NodeID), zap.Error(err))
				continue
			}
			log.Info("index meta recycle success", zap.Int64("buildID", segIdx.BuildID),
				zap.Int64("segmentID", segIdx.SegmentID))
		}
	}
}

// recycleUnusedIndexFiles is used to delete those index files that no longer exist in the meta.
func (gc *garbageCollector) recycleUnusedIndexFiles() {
	log.Info("start recycleUnusedIndexFiles")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTs := time.Now()
	prefix := path.Join(gc.option.cli.RootPath(), common.SegmentIndexPath) + "/"
	// list dir first
	objectPathHolderChan := gc.option.cli.ListWithPrefix(ctx, prefix, false)
	for objectPathHolder := range objectPathHolderChan {
		if objectPathHolder.Err != nil {
			log.Warn("garbageCollector recycleUnusedIndexFiles list keys from chunk manager failed",
				zap.String("prefix", prefix),
				zap.Error(objectPathHolder.Err))
			continue
		}
		key := objectPathHolder.Path
		log.Debug("indexFiles keys", zap.String("key", key))
		buildID, err := parseBuildIDFromFilePath(key)
		if err != nil {
			log.Warn("garbageCollector recycleUnusedIndexFiles parseIndexFileKey", zap.String("key", key), zap.Error(err))
			continue
		}
		log.Info("garbageCollector will recycle index files", zap.Int64("buildID", buildID))
		canRecycle, segIdx := gc.meta.indexMeta.CleanSegmentIndex(buildID)
		if !canRecycle {
			// Even if the index is marked as deleted, the index file will not be recycled, wait for the next gc,
			// and delete all index files about the buildID at one time.
			log.Info("garbageCollector can not recycle index files", zap.Int64("buildID", buildID))
			continue
		}
		if segIdx == nil {
			// buildID no longer exists in meta, remove all index files
			log.Info("garbageCollector recycleUnusedIndexFiles find meta has not exist, remove index files",
				zap.Int64("buildID", buildID))
			err = gc.option.cli.RemoveWithPrefix(ctx, key)
			if err != nil {
				log.Warn("garbageCollector recycleUnusedIndexFiles remove index files failed",
					zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
				continue
			}
			log.Info("garbageCollector recycleUnusedIndexFiles remove index files success",
				zap.Int64("buildID", buildID), zap.String("prefix", key))
			continue
		}
		filesMap := make(map[string]struct{})
		for _, fileID := range segIdx.IndexFileKeys {
			filepath := metautil.BuildSegmentIndexFilePath(gc.option.cli.RootPath(), segIdx.BuildID, segIdx.IndexVersion,
				segIdx.PartitionID, segIdx.SegmentID, fileID)
			filesMap[filepath] = struct{}{}
		}
		log.Info("start to recycle index files", zap.Int64("buildID", buildID), zap.Int("meta files num", len(filesMap)))
		recycleIndexPathHolderChan := gc.option.cli.ListWithPrefix(ctx, key, true)
		deletedFilesNum := 0
		for recycleIndexPathHolder := range recycleIndexPathHolderChan {
			if recycleIndexPathHolder.Err != nil {
				log.Warn("garbageCollector recycleUnusedIndexFiles list files failed",
					zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
				continue
			}
			file := recycleIndexPathHolder.Path
			if _, ok := filesMap[file]; !ok {
				if err = gc.option.cli.Remove(ctx, file); err != nil {
					log.Warn("garbageCollector recycleUnusedIndexFiles remove file failed",
						zap.Int64("buildID", buildID), zap.String("file", file), zap.Error(err))
					continue
				}
				deletedFilesNum++
			}
		}
		log.Info("index files recycle success", zap.Int64("buildID", buildID),
			zap.Int("delete index files num", deletedFilesNum))
	}
	log.Info("recycleUnusedIndexFiles, finish", zap.Duration("time spent", time.Since(startTs)))
}
