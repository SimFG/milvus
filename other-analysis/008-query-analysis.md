# Milvus 查询过程深度分析

## 1. 查询系统架构概述

Milvus 的查询系统采用分层架构设计，从上到下包括：
- **Proxy层**：接收用户请求，进行查询计划生成
- **QueryNode层**：执行查询计划，协调segment级别的查询
- **Segcore层**：底层执行引擎，负责实际的数据检索和向量搜索

## 2. 查询表达式的构建过程

### 2.1 表达式构建入口

查询表达式构建主要通过 `planparserv2` 包实现，核心文件：
- `/internal/parser/planparserv2/plan_parser_v2.go`

主要函数：
```go
// 创建检索计划
CreateRetrievePlan(schema, exprStr, exprTemplateValues) -> PlanNode

// 创建搜索计划  
CreateSearchPlan(schema, exprStr, vectorFieldName, queryInfo, exprTemplateValues) -> PlanNode
```

### 2.2 表达式类型支持

系统支持多种表达式类型：
- **标量表达式**：整数、浮点数、字符串、布尔值
- **列表达式**：字段引用，支持嵌套路径（JSON字段）
- **比较表达式**：等于、不等于、大于、小于、范围查询
- **逻辑表达式**：AND、OR、NOT
- **特殊表达式**：Term匹配、JSON包含、数组操作

## 3. 查询表达式的解析机制

### 3.1 解析流程

表达式解析采用 ANTLR 生成的解析器，流程如下：

1. **词法分析**：将表达式字符串转换为token流
   ```go
   inputStream := antlr.NewInputStream(exprNormal)
   lexer := getLexer(inputStream, listener)
   ```

2. **语法分析**：构建抽象语法树（AST）
   ```go
   parser := getParser(lexer, listener)
   ast = parser.Expr()
   ```

3. **语义分析**：通过Visitor模式遍历AST，生成执行计划
   ```go
   visitor := NewParserVisitor(schema)
   result := ast.Accept(visitor)
   ```

### 3.2 解析器优化

- **缓存机制**：使用LRU缓存已解析的表达式，避免重复解析
  ```go
  exprCache = expirable.NewLRU[string, any](1024, nil, time.Minute*10)
  ```

- **对象池**：复用lexer和parser对象，减少内存分配
  ```go
  putLexer(lexer)
  putParser(parser)
  ```

### 3.3 Visitor 模式实现

`ParserVisitor` 实现了对不同表达式节点的访问：

- `VisitIdentifier`: 处理字段标识符
- `VisitBoolean`: 处理布尔常量
- `VisitInteger`: 处理整数常量
- `VisitFloating`: 处理浮点数常量
- `VisitString`: 处理字符串常量
- `VisitParens`: 处理括号表达式

## 4. 查询执行流程

### 4.1 整体执行流程

```
用户请求 -> Proxy -> QueryNode -> Segcore -> 结果聚合
```

### 4.2 核心执行组件

#### 4.2.1 执行计划访问器
`ExecPlanNodeVisitor` (`/internal/core/src/query/ExecPlanNodeVisitor.cpp`)
- 负责遍历执行计划节点
- 协调segment级别的查询执行
- 管理查询上下文和结果收集

#### 4.2.2 执行算子

执行引擎包含多种算子（`/internal/core/src/exec/operator/`）：

- **VectorSearchNode**: 向量搜索算子
- **FilterBitsNode**: 过滤算子
- **GroupByNode**: 分组算子  
- **CountNode**: 计数算子
- **MvccNode**: MVCC过滤算子

#### 4.2.3 表达式执行器

表达式执行器（`/internal/core/src/exec/expression/`）：

- **CompareExpr**: 比较操作（=, !=, <, >, <=, >=）
- **TermExpr**: Term匹配操作
- **BinaryRangeExpr**: 范围查询
- **LogicalBinaryExpr**: 逻辑操作（AND, OR）
- **JsonContainsExpr**: JSON包含操作
- **ConjunctExpr**: 合取操作

### 4.3 查询执行步骤

1. **创建查询上下文**
   ```cpp
   auto query_context = std::make_shared<QueryContext>(
       query_id, segment, active_count, timestamp, ttl, consistency_level);
   ```

2. **构建执行计划片段**
   ```cpp
   auto plan = plan::PlanFragment(node.plannodes_);
   ```

3. **执行任务**
   ```cpp
   auto task = Task::Create(task_id, plan, 0, query_context);
   auto result = task->Next();
   ```

4. **收集结果**
   ```cpp
   search_result_opt_ = std::move(query_context->get_search_result());
   ```

## 5. 不同Segment类型的查询处理差异

### 5.1 Segment类型概述

Milvus中存在三种主要的Segment类型：
- **Growing Segment**: 实时插入的可变segment
- **Sealed Segment**: 已封存的不可变segment  
- **Chunked Sealed Segment**: 分块存储的sealed segment

### 5.2 Growing Segment 查询特点

文件：`/internal/core/src/segcore/SegmentGrowingImpl.cpp`

特点：
- **内存存储**：数据存储在内存中，访问速度快
- **实时更新**：支持实时插入和删除
- **无索引或临时索引**：通常没有完整索引，使用暴力搜索或临时索引
- **MVCC支持**：通过timestamp实现多版本并发控制

查询接口：
```cpp
SearchOnGrowing(const SegmentGrowingImpl& segment,
                const SearchInfo& info,
                const void* query_data,
                int64_t num_queries,
                Timestamp timestamp,
                const BitsetView& bitset,
                SearchResult& search_result);
```

### 5.3 Sealed Segment 查询特点

文件：`/internal/core/src/segcore/SegmentSealedImpl.cpp`

特点：
- **持久化存储**：数据可以在磁盘或内存映射文件中
- **索引完备**：拥有完整的向量索引和标量索引
- **高效查询**：利用索引加速查询
- **不可变**：数据不会改变，查询结果稳定

查询接口：
```cpp
// 基于索引的搜索
SearchOnSealedIndex(const Schema& schema,
                    const SealedIndexingRecord& record,
                    const SearchInfo& search_info,
                    const void* query_data,
                    int64_t num_queries,
                    const BitsetView& view,
                    SearchResult& search_result);

// 基于原始数据的搜索
SearchOnSealed(const Schema& schema,
               const void* vec_data,
               const SearchInfo& search_info,
               const std::map<std::string, std::string>& index_info,
               const void* query_data,
               int64_t num_queries,
               int64_t row_count,
               const BitsetView& bitset,
               SearchResult& result);
```

### 5.4 查询差异对比

| 特性 | Growing Segment | Sealed Segment |
|------|----------------|----------------|
| 数据可变性 | 可变 | 不可变 |
| 存储位置 | 内存 | 磁盘/内存映射 |
| 索引状态 | 无索引或临时索引 | 完整索引 |
| 查询性能 | 较慢（暴力搜索） | 较快（索引加速） |
| 删除处理 | 实时标记删除 | 预加载删除位图 |
| 主键查找 | 哈希表 | 有序数组二分查找 |
| 内存管理 | 动态增长 | 固定大小 |

### 5.5 统一查询接口

尽管底层实现不同，Milvus通过 `SegmentInterface` 提供统一的查询接口：

```cpp
class SegmentInterface {
    virtual void Search(const plan::PlanNode* plan,
                        const PlaceholderGroup* placeholder_group,
                        Timestamp timestamp,
                        SearchResult& result) = 0;
    
    virtual std::unique_ptr<RetrieveResult> 
    Retrieve(const plan::PlanNode* plan,
             Timestamp timestamp,
             int64_t limit_size,
             bool ignore_non_pk) = 0;
};
```

## 6. 查询优化机制

### 6.1 表达式缓存
- 使用LRU缓存避免重复解析表达式
- 缓存时间10分钟，容量1024个表达式

### 6.2 早期终止
- 当segment活跃数据为0时，直接返回空结果
- 避免不必要的计算开销

### 6.3 批量处理
- 查询执行采用批量处理模式
- 减少函数调用开销

### 6.4 索引选择
- Sealed segment优先使用索引查询
- Growing segment使用临时索引或暴力搜索

### 6.5 位图优化
- 使用位图快速过滤删除的数据
- 支持SIMD加速的位图操作

## 7. 查询结果处理

### 7.1 结果聚合
QueryNode收集各个segment的查询结果后，进行聚合处理：
- 合并多个segment的搜索结果
- 根据相似度重新排序
- 去重处理

### 7.2 结果缩减
Proxy层对多个QueryNode的结果进行最终缩减：
- 全局Top-K选择
- 结果格式化
- 返回给用户

## 8. 总结

Milvus的查询系统是一个复杂而高效的分布式查询引擎，主要特点包括：

1. **分层架构**：Proxy、QueryNode、Segcore三层清晰分工
2. **灵活的表达式系统**：支持丰富的查询表达式，使用ANTLR解析
3. **高效的执行引擎**：基于算子的执行模型，支持向量化执行
4. **差异化的Segment处理**：针对不同类型segment采用不同的查询策略
5. **完善的优化机制**：缓存、早期终止、索引选择等多种优化手段

这种设计使得Milvus能够同时支持高吞吐的批量查询和低延迟的实时查询，满足不同场景的需求。