# Milvus Streaming Channel 管理机制

## 1. 概述

Channel 是 Milvus Streaming 系统的核心抽象，负责：
- 数据流的逻辑分片
- 负载均衡的基本单位
- 流式处理的并行化
- 故障隔离和恢复

## 2. Channel 体系结构

### 2.1 Channel 类型

```go
// 物理 Channel (PChannel)
type PChannelInfo struct {
    Name       string        // Channel 名称
    Term       int64         // 任期号，用于 fencing
    AccessMode AccessMode    // 访问模式（RW/RO）
}

// 虚拟 Channel (VChannel)
type VChannelInfo struct {
    CollectionID int64       // 集合 ID
    VChannelName string      // 虚拟 Channel 名称
    PChannelName string      // 映射的物理 Channel
}
```

### 2.2 访问模式

```go
type AccessMode int32

const (
    AccessMode_RO AccessMode = 0  // 只读模式
    AccessMode_RW AccessMode = 1  // 读写模式
)
```

## 3. Channel Manager 实现

### 3.1 Manager 结构

```go
type ChannelManager struct {
    cond             *syncutil.ContextCond
    channels         map[ChannelID]*PChannelMeta   // Channel 元数据
    version          typeutil.VersionInt64Pair     // 版本控制
    metrics          *channelMetrics               // 监控指标
    streamingVersion *streamingpb.StreamingVersion // 流服务版本
    streamingEnableNotifiers []*syncutil.AsyncTaskNotifier[struct{}]
}
```

### 3.2 Channel 元数据

```go
type PChannelMeta struct {
    inner *streamingpb.PChannelMeta
}

type PChannelMeta struct {
    Channel    *PChannelInfo              // Channel 信息
    Node       *StreamingNodeInfo         // 分配的节点
    State      PChannelMetaState          // 状态
    Histories  []*PChannelAssignmentLog   // 历史记录
    LastAssignTimestampSeconds uint64     // 最后分配时间
}
```

### 3.3 Channel 状态机

```go
type PChannelMetaState int32

const (
    PCHANNEL_META_STATE_UNINITIALIZED PChannelMetaState = 0
    PCHANNEL_META_STATE_ASSIGNING     PChannelMetaState = 1
    PCHANNEL_META_STATE_ASSIGNED      PChannelMetaState = 2
    PCHANNEL_META_STATE_UNAVAILABLE   PChannelMetaState = 3
)
```

状态转换：
```
UNINITIALIZED ---> ASSIGNING ---> ASSIGNED
                       |              |
                       v              v
                   UNAVAILABLE <------+
```

## 4. Channel 恢复机制

### 4.1 恢复流程

```go
func RecoverChannelManager(ctx context.Context, 
                          incomingChannel ...string) (*ChannelManager, error) {
    // 1. 获取流服务版本
    streamingVersion, err := resource.Resource().StreamingCatalog().GetVersion(ctx)
    if err != nil {
        return nil, err
    }
    
    // 2. 从元数据恢复
    channels, metrics, err := recoverFromConfigurationAndMeta(
        ctx, streamingVersion, incomingChannel...)
    if err != nil {
        return nil, err
    }
    
    // 3. 创建 Manager
    return &ChannelManager{
        cond:     syncutil.NewContextCond(&sync.Mutex{}),
        channels: channels,
        version: typeutil.VersionInt64Pair{
            Global: paramtable.GetNodeID(),
            Local:  0,
        },
        metrics:          metrics,
        streamingVersion: streamingVersion,
    }, nil
}
```

### 4.2 元数据和配置合并

```go
func recoverFromConfigurationAndMeta(ctx context.Context, 
                                    streamingVersion *streamingpb.StreamingVersion,
                                    incomingChannel ...string) (
                                    map[ChannelID]*PChannelMeta, 
                                    *channelMetrics, error) {
    // 1. 从元数据获取已有 Channel
    channelMetas, err := resource.Resource().StreamingCatalog().ListPChannel(ctx)
    if err != nil {
        return nil, metrics, err
    }
    
    channels := make(map[ChannelID]*PChannelMeta)
    for _, channel := range channelMetas {
        c := newPChannelMetaFromProto(channel)
        channels[c.ChannelID()] = c
    }
    
    // 2. 处理新增 Channel
    for _, newChannel := range incomingChannel {
        var c *PChannelMeta
        if streamingVersion == nil {
            // 首次启动，设为只读
            c = newPChannelMeta(newChannel, types.AccessModeRO)
        } else {
            // 已启用流服务，设为读写
            c = newPChannelMeta(newChannel, types.AccessModeRW)
        }
        
        if _, ok := channels[c.ChannelID()]; !ok {
            channels[c.ChannelID()] = c
        }
    }
    
    return channels, metrics, nil
}
```

## 5. Channel 分配管理

### 5.1 分配操作

```go
func (m *mutablePChannel) TryAssignToServerID(
    accessMode types.AccessMode, 
    streamingNode types.StreamingNodeInfo) bool {
    
    // 1. 检查是否需要重新分配
    if m.ChannelInfo().AccessMode == accessMode && 
       m.CurrentServerID() == streamingNode.ServerID && 
       m.inner.State == PCHANNEL_META_STATE_ASSIGNED {
        return false
    }
    
    // 2. 保存历史记录
    if m.inner.State != PCHANNEL_META_STATE_UNINITIALIZED {
        m.inner.Histories = append(m.inner.Histories, &PChannelAssignmentLog{
            Term:       m.inner.Channel.Term,
            Node:       m.inner.Node,
            AccessMode: m.inner.Channel.AccessMode,
        })
    }
    
    // 3. 更新分配信息
    m.inner.Channel.AccessMode = streamingpb.PChannelAccessMode(accessMode)
    m.inner.Channel.Term++  // 增加任期号
    m.inner.Node = types.NewProtoFromStreamingNodeInfo(streamingNode)
    m.inner.State = PCHANNEL_META_STATE_ASSIGNING
    
    return true
}
```

### 5.2 分配确认

```go
func (m *mutablePChannel) AssignToServerDone() {
    if m.inner.State == PCHANNEL_META_STATE_ASSIGNING {
        // 清空历史记录（已成功分配）
        m.inner.Histories = make([]*streamingpb.PChannelAssignmentLog, 0)
        
        // 更新状态
        m.inner.State = PCHANNEL_META_STATE_ASSIGNED
        
        // 记录分配时间
        m.inner.LastAssignTimestampSeconds = uint64(time.Now().Unix())
    }
}
```

## 6. Channel 视图管理

### 6.1 PChannelView 结构

```go
type PChannelView struct {
    Version  typeutil.VersionInt64Pair
    Channels map[ChannelID]*PChannelMeta
}

func newPChannelView(channels map[ChannelID]*PChannelMeta) *PChannelView {
    // 深拷贝 channels
    copied := make(map[ChannelID]*PChannelMeta, len(channels))
    for id, channel := range channels {
        copied[id] = channel
    }
    
    return &PChannelView{
        Channels: copied,
    }
}
```

### 6.2 视图更新通知

```go
func (cm *ChannelManager) Watch(ctx context.Context, 
                               watcher func(view *PChannelView) error) error {
    // 1. 获取初始视图
    view := cm.CurrentPChannelsView()
    if err := watcher(view); err != nil {
        return err
    }
    
    // 2. 监听变更
    lastVersion := view.Version
    for {
        cm.cond.L.Lock()
        
        // 等待版本更新
        for cm.version.EQ(lastVersion) && ctx.Err() == nil {
            cm.cond.Wait(ctx)
        }
        
        if ctx.Err() != nil {
            cm.cond.L.Unlock()
            return ctx.Err()
        }
        
        // 3. 通知新视图
        newView := newPChannelView(cm.channels)
        newView.Version = cm.version
        cm.cond.L.Unlock()
        
        if err := watcher(newView); err != nil {
            return err
        }
        
        lastVersion = newView.Version
    }
}
```

## 7. 流服务启用管理

### 7.1 启用通知机制

```go
func (cm *ChannelManager) RegisterStreamingEnabledNotifier(
    notifier *syncutil.AsyncTaskNotifier[struct{}]) {
    
    cm.cond.L.Lock()
    defer cm.cond.L.Unlock()
    
    if cm.streamingVersion != nil {
        // 已启用，立即通知
        notifier.Cancel()
        return
    }
    
    // 注册等待通知
    cm.streamingEnableNotifiers = append(cm.streamingEnableNotifiers, notifier)
}
```

### 7.2 标记流服务已启用

```go
func (cm *ChannelManager) MarkStreamingHasEnabled(ctx context.Context) error {
    cm.cond.L.Lock()
    defer cm.cond.L.Unlock()
    
    // 1. 更新版本
    cm.streamingVersion = &streamingpb.StreamingVersion{
        Version: 1,
    }
    
    // 2. 持久化版本
    if err := retry.Do(ctx, func() error {
        return resource.Resource().StreamingCatalog().SaveVersion(
            ctx, cm.streamingVersion)
    }, retry.AttemptAlways()); err != nil {
        return err
    }
    
    // 3. 通知所有等待者
    for _, notifier := range cm.streamingEnableNotifiers {
        notifier.Cancel()
    }
    
    // 4. 等待通知完成
    for _, notifier := range cm.streamingEnableNotifiers {
        notifier.BlockUntilFinish()
    }
    
    cm.streamingEnableNotifiers = nil
    return nil
}
```

## 8. Channel 操作接口

### 8.1 基本操作

```go
// 分配 Channel
func (cm *ChannelManager) AssignPChannels(ctx context.Context, 
    updates map[ChannelID]*types.PChannelInfoAssigned) error {
    
    cm.cond.LockAndBroadcast()
    defer cm.cond.L.Unlock()
    
    // 应用更新
    for id, update := range updates {
        channel := cm.channels[id]
        if channel == nil {
            return ErrChannelNotExist
        }
        
        mutableChannel := channel.CopyForWrite()
        if mutableChannel.TryAssignToServerID(
            update.Channel.AccessMode, update.Node) {
            
            // 更新到管理器
            cm.channels[id] = mutableChannel.PChannelMeta
            
            // 持久化
            if err := cm.persistAssignment(ctx, mutableChannel); err != nil {
                return err
            }
        }
    }
    
    // 更新版本
    cm.version.Local++
    
    return nil
}

// 移除 Channel
func (cm *ChannelManager) RemovePChannels(ctx context.Context, 
    channelIDs ...ChannelID) error {
    
    cm.cond.LockAndBroadcast()
    defer cm.cond.L.Unlock()
    
    for _, id := range channelIDs {
        channel := cm.channels[id]
        if channel == nil {
            continue
        }
        
        // 标记为不可用
        mutableChannel := channel.CopyForWrite()
        mutableChannel.MarkAsUnavailable()
        
        cm.channels[id] = mutableChannel.PChannelMeta
        
        // 持久化状态
        if err := cm.persistUnavailable(ctx, mutableChannel); err != nil {
            return err
        }
    }
    
    cm.version.Local++
    
    return nil
}
```

### 8.2 查询操作

```go
// 获取 Channel 的节点位置
func (cm *ChannelManager) FindPChannelNodeLocation(channel ChannelID) (int64, bool) {
    cm.cond.L.Lock()
    defer cm.cond.L.Unlock()
    
    if meta, ok := cm.channels[channel]; ok && meta.IsAssigned() {
        return meta.CurrentServerID(), true
    }
    
    return -1, false
}

// 获取节点的所有 Channel
func (cm *ChannelManager) GetNodeChannels(nodeID int64) []*PChannelMeta {
    cm.cond.L.Lock()
    defer cm.cond.L.Unlock()
    
    var channels []*PChannelMeta
    for _, channel := range cm.channels {
        if channel.CurrentServerID() == nodeID {
            channels = append(channels, channel)
        }
    }
    
    return channels
}
```

## 9. 监控指标

### 9.1 Channel 状态指标

```go
type channelMetrics struct {
    // 按状态统计
    uninitializedCount prometheus.Gauge
    assigningCount     prometheus.Gauge
    assignedCount      prometheus.Gauge
    unavailableCount   prometheus.Gauge
    
    // 按节点统计
    nodeChannelCount *prometheus.GaugeVec
}

func (m *channelMetrics) AssignPChannelStatus(channel *PChannelMeta) {
    switch channel.State() {
    case PCHANNEL_META_STATE_UNINITIALIZED:
        m.uninitializedCount.Inc()
    case PCHANNEL_META_STATE_ASSIGNING:
        m.assigningCount.Inc()
    case PCHANNEL_META_STATE_ASSIGNED:
        m.assignedCount.Inc()
        m.nodeChannelCount.WithLabelValues(
            fmt.Sprintf("%d", channel.CurrentServerID())).Inc()
    case PCHANNEL_META_STATE_UNAVAILABLE:
        m.unavailableCount.Inc()
    }
}
```

### 9.2 操作延迟指标

```go
// 分配延迟
StreamingCoordChannelAssignLatency

// 视图更新延迟  
StreamingCoordViewUpdateLatency

// 持久化延迟
StreamingCoordPersistLatency
```

## 10. 最佳实践

### 10.1 Channel 数量规划

```yaml
streaming:
  channel:
    # 每个节点的 Channel 数量
    channelsPerNode: 16
    
    # Channel 名称前缀
    namePrefix: "streaming-channel-"
    
    # 最大 Channel 数量
    maxChannels: 1024
```

### 10.2 分配策略配置

```yaml
balancer:
  policy: "vchannel_fair"  # 负载均衡策略
  
  # 重平衡参数
  rebalance:
    threshold: 0.2         # 不平衡阈值
    minInterval: 30s       # 最小重平衡间隔
```

### 10.3 故障处理

1. **节点故障**
   - 自动检测节点离线
   - 重新分配受影响的 Channel
   - 更新 Channel Term 防止旧节点写入

2. **网络分区**
   - 使用 Term 机制防止脑裂
   - 基于 etcd 的一致性保证
   - 自动恢复机制

3. **数据保护**
   - Channel 元数据多副本
   - 操作日志持久化
   - 定期快照备份