# VectorMemIndex 与 VectorDiskIndex 对比分析

## 1. 功能定位差异

### VectorMemIndex
- **内存索引**：数据和索引结构完全加载到内存中进行操作
- **支持增量操作**：支持 `AddWithDataset` 方法动态添加数据
- **适用场景**：用于 Growing Segment 和需要频繁更新的场景
- **文件管理器**：使用 `MemFileManagerImpl` 管理内存中的索引数据
- **源文件位置**：
  - `/internal/core/src/index/VectorMemIndex.h`
  - `/internal/core/src/index/VectorMemIndex.cpp`

### VectorDiskAnnIndex
- **磁盘索引**：索引主要存储在磁盘上，查询时按需加载
- **不支持增量**：无 `AddWithDataset` 方法，只支持一次性构建
- **适用场景**：用于 Sealed Segment，处理大规模静态数据
- **文件管理器**：使用 `DiskFileManagerImpl` 管理磁盘文件
- **源文件位置**：
  - `/internal/core/src/index/VectorDiskIndex.h`
  - `/internal/core/src/index/VectorDiskIndex.cpp`

## 2. 类定义对比

### VectorMemIndex 类定义
```cpp
template <typename T>
class VectorMemIndex : public VectorIndex {
 public:
    // 支持两种构造函数
    explicit VectorMemIndex(
        const IndexType& index_type,
        const MetricType& metric_type,
        const IndexVersion& version,
        bool use_knowhere_build_pool = true,
        const storage::FileManagerContext& file_manager_context);
    
    // 特殊构造函数：用于 Growing Segment，支持 ViewDataOp
    VectorMemIndex(const IndexType& index_type,
                   const MetricType& metric_type,
                   const IndexVersion& version,
                   bool use_knowhere_build_pool,
                   const knowhere::ViewDataOp view_data);
    
    // 支持增量添加
    void AddWithDataset(const DatasetPtr& dataset, const Config& config);
    
 protected:
    Config config_;
    knowhere::Index<knowhere::IndexNode> index_;
    std::shared_ptr<storage::MemFileManagerImpl> file_manager_;
    bool use_knowhere_build_pool_;
};
```

### VectorDiskAnnIndex 类定义
```cpp
template <typename T>
class VectorDiskAnnIndex : public VectorIndex {
 public:
    explicit VectorDiskAnnIndex(
        const IndexType& index_type,
        const MetricType& metric_type,
        const IndexVersion& version,
        const storage::FileManagerContext& file_manager_context);
    
    // 特有方法：清理本地缓存
    void CleanLocalData() override;
    
    // 不支持获取稀疏向量
    std::unique_ptr<const knowhere::sparse::SparseRow<float>[]>
    GetSparseVector(const DatasetPtr dataset) const override {
        PanicInfo(ErrorCode::Unsupported,
                  "get sparse vector not supported for disk index");
    }
    
 private:
    knowhere::Index<knowhere::IndexNode> index_;
    std::shared_ptr<storage::DiskFileManagerImpl> file_manager_;
    uint32_t search_beamwidth_ = 8;  // 磁盘访问优化参数
};
```

## 3. 核心方法实现对比

### 3.1 数据加载 (Load)

#### VectorMemIndex::Load
```cpp
// 支持两种加载方式
void VectorMemIndex<T>::Load(const BinarySet& binary_set, const Config& config) {
    // 方式1：从 BinarySet 加载
    milvus::Assemble(const_cast<BinarySet&>(binary_set));
    LoadWithoutAssemble(binary_set, config);
}

void VectorMemIndex<T>::Load(milvus::tracer::TraceContext ctx, const Config& config) {
    // 方式2：从文件加载
    // 1. 批量读取索引文件到内存
    // 2. 支持分片(slice)机制
    // 3. 组装 BinarySet
    // 4. 反序列化到 Knowhere 索引
    
    if (slice_meta_filepath) {
        // 批量加载分片数据
        for (auto& batch : batches) {
            auto batch_data = file_manager_->LoadIndexToMemory(batch);
            // 组装数据...
        }
    }
    
    BinarySet binary_set;
    AssembleIndexDatas(index_data_codecs, binary_set);
    LoadWithoutAssemble(binary_set, config);
}
```

#### VectorDiskAnnIndex::Load
```cpp
void VectorDiskAnnIndex<T>::Load(milvus::tracer::TraceContext ctx,
                                 const Config& config) {
    // 直接从磁盘加载，不需要组装 BinarySet
    knowhere::Json load_config = update_load_json(config);
    
    // 1. 缓存索引文件到本地磁盘
    auto index_files = GetValueFromConfig<std::vector<std::string>>(config, "index_files");
    file_manager_->CacheIndexToDisk(index_files.value());
    
    // 2. Knowhere 直接从磁盘文件读取
    auto stat = index_.Deserialize(knowhere::BinarySet(), load_config);
    
    SetDim(index_.Dim());
}
```

### 3.2 索引构建 (Build)

#### VectorMemIndex::Build
```cpp
void VectorMemIndex<T>::Build(const Config& config) {
    // 1. 从文件读取原始数据到内存
    auto field_datas = file_manager_->CacheRawDataToMemory(insert_files.value());
    
    // 2. 拼接所有数据到连续内存块
    auto buf = std::shared_ptr<uint8_t[]>(new uint8_t[total_size]);
    for (auto data : field_datas) {
        std::memcpy(buf.get() + offset, data->Data(), data->Size());
        offset += data->Size();
    }
    
    // 3. 创建 Dataset 并构建索引
    auto dataset = GenDataset(total_num_rows, dim, buf.get());
    BuildWithDataset(dataset, build_config);
}

void VectorMemIndex<T>::BuildWithDataset(const DatasetPtr& dataset,
                                         const Config& config) {
    SetDim(dataset->GetDim());
    auto stat = index_.Build(dataset, index_config, use_knowhere_build_pool_);
}
```

#### VectorDiskAnnIndex::Build
```cpp
void VectorDiskAnnIndex<T>::Build(const Config& config) {
    // 1. 将原始数据缓存到本地磁盘
    auto local_data_path = 
        file_manager_->CacheRawDataToDisk<T>(insert_files.value());
    
    // 2. 设置磁盘路径参数
    build_config[DISK_ANN_RAW_DATA_PATH] = local_data_path;
    build_config[DISK_ANN_PREFIX_PATH] = local_index_path_prefix;
    
    // 3. 配置线程数等参数
    if (GetIndexType() == knowhere::IndexEnum::INDEX_DISKANN) {
        build_config[DISK_ANN_THREADS_NUM] = 
            std::atoi(num_threads.value().c_str());
    }
    
    // 4. Knowhere 从磁盘读取并构建
    auto stat = index_.Build({}, build_config);
    
    // 5. 清理临时原始数据
    local_chunk_manager->RemoveDir(
        storage::GetSegmentRawDataPathPrefix(local_chunk_manager, segment_id));
}
```

### 3.3 查询 (Query)

两者查询接口相同，但底层实现不同：

```cpp
void Query(const DatasetPtr dataset,
          const SearchInfo& search_info,
          const BitsetView& bitset,
          SearchResult& search_result) const override;
```

- **VectorMemIndex**：直接在内存索引结构上查询，延迟低
- **VectorDiskAnnIndex**：可能需要从磁盘加载部分数据，支持 `search_beamwidth` 参数优化磁盘访问

## 4. 处理流程差异

### 内存索引流程
```
原始数据 → 加载到内存 → 构建内存索引 → 序列化保存 → 查询时全部在内存
```

### 磁盘索引流程
```
原始数据 → 写入磁盘文件 → 构建磁盘索引 → 索引存储在磁盘 → 查询时按需加载
```

## 5. 特有功能对比

### VectorMemIndex 特有功能

#### 1. 增量添加数据
```cpp
void VectorMemIndex<T>::AddWithDataset(const DatasetPtr& dataset,
                                       const Config& config) {
    knowhere::Json index_config;
    index_config.update(config);
    
    auto stat = index_.Add(dataset, index_config, use_knowhere_build_pool_);
    if (stat != knowhere::Status::success)
        PanicInfo(ErrorCode::IndexBuildError,
                  "failed to append index");
}
```

#### 2. ViewDataOp 支持（避免数据复制）
用于 Growing Segment，直接引用原始数据：
```cpp
knowhere::ViewDataOp view_data = [field_raw_data_ptr](size_t id) {
    return (const void*)field_raw_data_ptr->get_element(id);
};
```

#### 3. 分片加载机制
支持将大索引文件分片加载：
```cpp
if (!slice_meta_filepath.empty()) {
    // 读取分片元信息
    Config meta_data = Config::parse(raw_slice_meta);
    for (auto& item : meta_data[META]) {
        // 批量加载分片
        auto batch_data = file_manager_->LoadIndexToMemory(batch);
    }
}
```

### VectorDiskAnnIndex 特有功能

#### 1. 本地缓存清理
```cpp
void CleanLocalData() override {
    // 清理本地缓存的索引数据
    local_chunk_manager->RemoveDir(local_index_path_prefix);
}
```

#### 2. 磁盘优化参数
```cpp
private:
    uint32_t search_beamwidth_ = 8;  // 控制磁盘访问的宽度
    
    knowhere::Json update_load_json(const Config& config) {
        // 更新加载配置，优化磁盘访问
    }
```

#### 3. 直接文件映射
不需要加载到内存，直接使用文件映射：
```cpp
// 创建时传递 FileManager 给 Knowhere
auto diskann_index_pack = 
    knowhere::Pack(std::shared_ptr<knowhere::FileManager>(file_manager_));
```

## 6. 存储管理差异

### VectorMemIndex 存储特点
- 使用 `MemFileManagerImpl` 管理内存数据
- 支持序列化/反序列化到 BinarySet
- 数据完全加载到内存
- 支持分片管理大文件

### VectorDiskAnnIndex 存储特点
- 使用 `DiskFileManagerImpl` 管理磁盘文件
- 索引文件直接存储在磁盘
- 最小化内存占用
- 支持本地缓存管理

## 7. 性能特征对比

| 特性 | VectorMemIndex | VectorDiskAnnIndex |
|-----|---------------|-------------------|
| 查询延迟 | 低（纳秒级） | 较高（需要磁盘 I/O） |
| 内存占用 | 高（全部加载） | 低（按需加载） |
| 数据规模支持 | 受内存限制 | 支持超大规模 |
| 增量更新 | 支持 | 不支持 |
| 适用场景 | 实时查询、高 QPS | 大规模静态数据 |
| 索引构建速度 | 快 | 较慢 |
| 持久化成本 | 需要序列化 | 直接持久化 |

## 8. 使用建议

### 选择 VectorMemIndex 的场景
1. Growing Segment（实时数据插入）
2. 数据规模在内存容量范围内
3. 需要极低的查询延迟
4. 需要支持增量更新
5. 高 QPS 查询场景

### 选择 VectorDiskAnnIndex 的场景
1. Sealed Segment 且数据规模巨大
2. 内存资源有限
3. 可以接受稍高的查询延迟
4. 数据相对静态，不需要频繁更新
5. 成本敏感的大规模部署

## 9. 代码示例

### 创建 VectorMemIndex
```cpp
// Growing Segment 中使用
auto index = std::make_unique<index::VectorMemIndex<float>>(
    knowhere::IndexEnum::INDEX_FAISS_IVFFLAT,
    knowhere::metric::L2,
    knowhere::Version::GetCurrentVersion().VersionNumber(),
    false,  // use_knowhere_build_pool
    view_data  // 避免数据复制
);
```

### 创建 VectorDiskAnnIndex
```cpp
// Sealed Segment 中使用
auto index = std::make_unique<VectorDiskAnnIndex<float>>(
    knowhere::IndexEnum::INDEX_DISKANN,
    metric_type,
    version,
    file_manager_context
);
```

这种双索引设计使得 Milvus 能够灵活应对不同的使用场景，在性能和资源使用之间找到最佳平衡点。