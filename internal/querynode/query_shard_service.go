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

package querynode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/dependency"
	"go.uber.org/zap"
)

// TODO, remove queryShardService, it's not used any more.
type queryShardService struct {
	ctx    context.Context
	cancel context.CancelFunc

	queryShardsMu sync.Mutex              // guards queryShards
	queryShards   map[Channel]*queryShard // Virtual Channel -> *queryShard

	factory dependency.Factory

	metaReplica  ReplicaInterface
	tSafeReplica TSafeReplicaInterface

	shardClusterService *ShardClusterService
	localChunkManager   storage.ChunkManager
	remoteChunkManager  storage.ChunkManager
	localCacheEnabled   bool
	scheduler           *taskScheduler
}

func newQueryShardService(ctx context.Context, metaReplica ReplicaInterface, tSafeReplica TSafeReplicaInterface, clusterService *ShardClusterService, factory dependency.Factory, scheduler *taskScheduler) (*queryShardService, error) {
	// TODO we don't need the local chunk manager any more
	localChunkManager := storage.NewLocalChunkManager(storage.RootPath(Params.LocalStorageCfg.Path))
	remoteChunkManager, err := factory.NewPersistentStorageChunkManager(ctx)
	if err != nil {
		log.Ctx(ctx).Warn("failed to init remote chunk manager", zap.Error(err))
		return nil, err
	}
	queryShardServiceCtx, queryShardServiceCancel := context.WithCancel(ctx)
	qss := &queryShardService{
		ctx:                 queryShardServiceCtx,
		cancel:              queryShardServiceCancel,
		queryShards:         make(map[Channel]*queryShard),
		metaReplica:         metaReplica,
		tSafeReplica:        tSafeReplica,
		shardClusterService: clusterService,
		localChunkManager:   localChunkManager,
		remoteChunkManager:  remoteChunkManager,
		localCacheEnabled:   Params.QueryNodeCfg.CacheEnabled,
		factory:             factory,
		scheduler:           scheduler,
	}
	return qss, nil
}

func (q *queryShardService) addQueryShard(collectionID UniqueID, channel Channel, replicaID int64, delta int64) error {
	log := log.With(
		zap.Int64("collection", collectionID),
		zap.Int64("replica", replicaID),
		zap.String("channel", channel),
		zap.Int64("delta", delta),
	)
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()
	if qs, ok := q.queryShards[channel]; ok {
		qs.inUse.Add(delta)
		log.Info("Successfully add query shard delta")
		return nil
	}
	qs, err := newQueryShard(
		q.ctx,
		collectionID,
		channel,
		replicaID,
		q.shardClusterService,
		q.metaReplica,
		q.tSafeReplica,
		q.localChunkManager,
		q.remoteChunkManager,
		q.localCacheEnabled,
	)
	if err != nil {
		return err
	}
	qs.inUse.Add(delta)
	q.queryShards[channel] = qs
	log.Info("Successfully add new query shard")
	return nil
}

func (q *queryShardService) removeQueryShard(channel Channel, delta int64) error {
	log := log.With(
		zap.String("channel", channel),
		zap.Int64("delta", delta),
	)
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()
	qs, ok := q.queryShards[channel]
	if !ok {
		return errors.New(fmt.Sprintln("query shard(channel) ", channel, " does not exist"))
	}
	inUse := qs.inUse.Add(-delta)
	if inUse == 0 {
		delete(q.queryShards, channel)
		qs.Close()
		log.Info("Successfully remove query shard")
		return nil
	}
	log.Info("Successfully remove query shard inUse")
	return nil
}

func (q *queryShardService) hasQueryShard(channel Channel) bool {
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()
	_, found := q.queryShards[channel]
	return found
}

func (q *queryShardService) getQueryShard(channel Channel) (*queryShard, error) {
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()
	if _, ok := q.queryShards[channel]; !ok {
		log.Info("debug strack", zap.String("channel", channel), zap.Stack("channel_not_exist"))
		return nil, errors.New(fmt.Sprintln("query shard(channel) ", channel, " does not exist"))
	}
	return q.queryShards[channel], nil
}

func (q *queryShardService) close() {
	log.Warn("Close query shard service")
	q.cancel()
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()

	for channel, queryShard := range q.queryShards {
		queryShard.Close()
		delete(q.queryShards, channel)
	}
}

func (q *queryShardService) releaseCollection(collectionID int64) {
	q.queryShardsMu.Lock()
	for channel, queryShard := range q.queryShards {
		if queryShard.collectionID == collectionID {
			queryShard.Close()
			delete(q.queryShards, channel)
		}
	}
	q.queryShardsMu.Unlock()
	log.Info("release collection in query shard service", zap.Int64("collectionId", collectionID))
}

func (q *queryShardService) Num() int {
	q.queryShardsMu.Lock()
	defer q.queryShardsMu.Unlock()
	return len(q.queryShards)
}

func (q *queryShardService) Empty() bool {
	return q.Num() == 0
}
