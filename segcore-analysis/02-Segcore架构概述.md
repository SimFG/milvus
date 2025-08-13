# Milvus Segcore 架构概述

## 核心概念

Segcore 是 Milvus 的核心执行引擎，负责数据的存储、索引构建和查询执行。它的设计基于以下几个核心概念：

### 1. Collection（集合）
- Collection 是 Milvus 中的表概念，对应关系数据库中的表
- 包含 Schema（模式定义）和 IndexMeta（索引元信息）
- 是所有 Segment 的容器

### 2. Segment（段）
Segment 是数据存储和查询的基本单位，分为两种类型：

#### 2.1 Growing Segment（增长段）
- 用于接收新插入的数据
- 支持实时插入和删除操作
- 数据按 chunk（块）组织，每个 chunk 默认包含一定数量的行
- 当达到一定条件后会被封存（seal）成 Sealed Segment

#### 2.2 Sealed Segment（封存段）
- 只读段，不再接收新的插入
- 支持更高效的索引和查询
- 有两种实现：
  - `SegmentSealedImpl`：普通的封存段
  - `ChunkedSegmentSealedImpl`：分块的封存段，支持更大规模的数据

## 核心组件架构

### 1. SegmentInterface 层次结构

```
SegmentInterface (抽象接口)
    ├── 基本操作接口
    │   ├── Search() - 向量搜索
    │   ├── Retrieve() - 数据检索
    │   ├── Delete() - 删除操作
    │   └── LoadFieldData() - 加载字段数据
    │
    └── SegmentInternalInterface (内部接口)
        ├── SegmentGrowing (增长段接口)
        │   └── SegmentGrowingImpl
        │
        └── SegmentSealed (封存段接口)
            ├── SegmentSealedImpl
            └── ChunkedSegmentSealedImpl
```

### 2. 数据组织结构

#### 2.1 InsertRecord（插入记录）
- 管理 Growing Segment 中的原始数据
- 按字段组织数据，每个字段的数据存储在 ConcurrentVector 中
- 支持并发插入

#### 2.2 DeletedRecord（删除记录）
- 记录被删除的主键和时间戳
- 用于在查询时过滤已删除的数据

#### 2.3 IndexingRecord（索引记录）
- 管理 Growing Segment 中的索引构建
- 支持增量索引构建

### 3. 查询执行架构

#### 3.1 查询计划（Plan）
- 由上层（Go 层）生成的查询计划
- 包含过滤条件、输出字段等信息

#### 3.2 执行引擎（Exec）
- `Driver`：执行驱动器，负责协调整个查询执行过程
- `Task`：执行任务的抽象
- `Expression`：表达式计算，支持各种标量过滤条件
- `Operator`：执行算子，如 VectorSearchNode、FilterBitsNode 等

#### 3.3 搜索策略
- `SearchOnGrowing`：在 Growing Segment 上执行搜索
- `SearchOnSealed`：在 Sealed Segment 上执行搜索
- `SearchOnIndex`：基于索引的搜索

### 4. 索引体系

#### 4.1 标量索引
- `ScalarIndex`：标量索引基类
- `ScalarIndexSort`：基于排序的标量索引
- `StringIndexMarisa`：字符串索引（使用 Marisa Trie）
- `BitmapIndex`：位图索引
- `InvertedIndexTantivy`：倒排索引（基于 Tantivy）

#### 4.2 向量索引
- `VectorMemIndex`：内存中的向量索引
- `VectorDiskIndex`：磁盘上的向量索引
- 底层使用 Knowhere 库实现各种向量索引算法

### 5. 存储层

#### 5.1 ChunkManager（块管理器）
- 抽象的存储接口
- 支持多种存储后端：
  - `LocalChunkManager`：本地文件系统
  - `MinioChunkManager`：MinIO 对象存储
  - `AzureChunkManager`：Azure Blob 存储
  - `GcpNativeChunkManager`：Google Cloud Storage

#### 5.2 数据编码
- `DataCodec`：数据编解码器
- `PayloadReader/Writer`：数据读写器
- 支持数据压缩和序列化

## 请求处理流程

1. **数据插入流程**
   - 请求到达 Growing Segment
   - PreInsert 预分配空间
   - Insert 写入数据到 InsertRecord
   - 更新索引（如果需要）

2. **查询执行流程**
   - 解析查询计划
   - 在各个 Segment 上并行执行查询
   - 合并各 Segment 的结果
   - 应用时间戳过滤和删除过滤
   - 返回最终结果

3. **索引构建流程**
   - Growing Segment 中的增量索引构建
   - Sealed Segment 的批量索引构建
   - 索引加载和缓存管理

## 并发控制

- 使用 `std::shared_mutex` 实现读写锁
- Growing Segment 使用 `ConcurrentVector` 支持并发插入
- 使用 `tbb::concurrent_unordered_map` 等并发数据结构

## 内存管理

- 通过 `SegmentStats` 跟踪内存使用
- 支持 mmap 方式加载大文件
- 实现了 Chunk 级别的内存管理和释放