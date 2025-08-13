# Milvus 执行算子与表达式执行器关系详解

## 1. 概念澄清

在Milvus的查询执行引擎中，存在两个容易混淆的概念：

### 1.1 执行算子（Operator）
- **定义**：执行算子是查询计划的执行单元，负责完成特定的数据处理任务
- **位置**：`/internal/core/src/exec/operator/`
- **特点**：
  - 实现数据流处理逻辑
  - 管理输入输出数据流
  - 协调整个查询执行过程

### 1.2 表达式执行器（Expression Executor）
- **定义**：表达式执行器负责计算具体的表达式，如比较、逻辑运算等
- **位置**：`/internal/core/src/exec/expression/`
- **特点**：
  - 实现具体的表达式计算逻辑
  - 处理字段访问和值计算
  - 返回布尔结果或其他计算值

## 2. 架构关系

### 2.1 层级关系图

```
┌─────────────────────────────────────────┐
│           Task (任务层)                  │
│  - 管理整个查询执行                       │
│  - 创建和调度Driver                      │
└─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────┐
│          Driver (驱动层)                 │
│  - 执行算子的运行时环境                   │
│  - 管理算子执行流程                       │
└─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────┐
│         Operator (算子层)                │
│  - FilterBitsNode：过滤算子              │
│  - VectorSearchNode：向量搜索算子         │
│  - GroupByNode：分组算子                 │
│  - MvccNode：MVCC过滤算子                │
└─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────┐
│      ExprSet (表达式集合)                │
│  - 管理一组表达式                        │
│  - 协调表达式执行                        │
└─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────┐
│    Expression (表达式执行器层)            │
│  - CompareExpr：比较表达式               │
│  - TermExpr：Term匹配表达式              │
│  - LogicalBinaryExpr：逻辑表达式         │
│  - JsonContainsExpr：JSON包含表达式      │
│  - CallExpr：函数调用表达式              │
└─────────────────────────────────────────┘
```

### 2.2 关键关系说明

1. **算子包含表达式**：执行算子内部包含表达式执行器来完成具体的计算
2. **算子管理数据流**：算子负责数据的输入输出和流转
3. **表达式执行计算**：表达式执行器负责具体的值计算和条件判断

## 3. 调用关系详解

### 3.1 FilterBitsNode 算子示例

以 `FilterBitsNode` 为例，展示算子如何使用表达式：

```cpp
// FilterBitsNode.cpp
class PhyFilterBitsNode : public Operator {
private:
    std::unique_ptr<ExprSet> exprs_;  // 包含表达式集合
    
public:
    PhyFilterBitsNode(...) {
        // 初始化时创建表达式集合
        exprs_ = std::make_unique<ExprSet>(filters, exec_context);
    }
    
    RowVectorPtr GetOutput() {
        // 创建表达式执行上下文
        EvalCtx eval_ctx(operator_context_->get_exec_context(), exprs_.get());
        
        // 调用表达式执行
        exprs_->Eval(0, 1, true, eval_ctx, results_);
        
        // 处理表达式结果
        // ...
    }
};
```

**调用流程**：
1. 算子初始化时创建 `ExprSet`
2. 执行时创建 `EvalCtx` 上下文
3. 调用 `exprs_->Eval()` 执行表达式
4. 获取并处理表达式执行结果

### 3.2 VectorSearchNode 算子示例

```cpp
class PhyVectorSearchNode : public Operator {
    RowVectorPtr GetOutput() {
        // 获取过滤后的位图（来自前置的FilterBitsNode）
        auto col_input = GetColumnVector(input_);
        TargetBitmapView view(col_input->GetRawData(), col_input->size());
        
        // 使用位图进行向量搜索
        segment_->vector_search(search_info_,
                               src_data,
                               num_queries,
                               query_timestamp_,
                               final_view,
                               search_result);
    }
};
```

**关系说明**：
- VectorSearchNode 通常接收 FilterBitsNode 的输出
- FilterBitsNode 使用表达式过滤数据，生成位图
- VectorSearchNode 使用位图进行向量搜索

## 4. ExprSet 的桥梁作用

### 4.1 ExprSet 职责

`ExprSet` 是算子和表达式之间的桥梁：

```cpp
class ExprSet {
    std::vector<std::shared_ptr<Expr>> exprs_;  // 表达式列表
    
public:
    // 编译逻辑表达式为物理表达式
    explicit ExprSet(const std::vector<expr::TypedExprPtr>& logical_exprs,
                     ExecContext* exec_ctx) {
        exprs_ = CompileExpressions(logical_exprs, exec_ctx);
    }
    
    // 执行表达式
    void Eval(int32_t begin,
             int32_t end,
             bool initialize,
             EvalCtx& ctx,
             std::vector<VectorPtr>& result);
};
```

### 4.2 表达式编译过程

```
逻辑表达式（来自查询计划）
    ↓
CompileExpressions（编译转换）
    ↓
物理表达式（Expr子类实例）
    ↓
执行时调用 Eval() 方法
```

## 5. 表达式执行器类型

### 5.1 基础表达式类

```cpp
class Expr {
public:
    // 核心执行方法
    virtual void Eval(EvalCtx& context, VectorPtr& result);
    
    // 是否是数据源表达式
    virtual bool IsSource() const;
    
    // 移动游标（批处理优化）
    virtual void MoveCursor();
};
```

### 5.2 具体表达式实现

#### SegmentExpr（段表达式）
- 直接访问segment数据
- 实现字段数据读取
- 支持索引和原始数据访问

#### CompareExpr（比较表达式）
- 实现各种比较操作（=, !=, <, >, <=, >=）
- 支持向量化执行
- 优化的批量比较

#### TermExpr（Term表达式）
- 实现 IN 操作
- 支持集合匹配
- 优化的哈希查找

#### CallExpr（函数调用表达式）
- 支持用户定义函数
- 通过 FunctionFactory 注册
- 灵活的扩展机制

## 6. 执行流程示例

### 6.1 完整查询执行流程

以一个包含过滤和向量搜索的查询为例：

```sql
-- 伪SQL表示
SELECT * FROM collection 
WHERE age > 18 AND city = "Beijing"
ORDER BY SIMILARITY(vector, query_vector)
LIMIT 10
```

执行流程：

```
1. Task创建
   └─> 创建 Driver
   
2. Driver执行
   └─> FilterBitsNode (age > 18 AND city = "Beijing")
       ├─> ExprSet 包含两个表达式
       ├─> CompareExpr(age > 18)
       └─> CompareExpr(city = "Beijing")
   
3. 表达式执行
   ├─> SegmentExpr 读取 age 字段数据
   ├─> CompareExpr 执行 > 18 比较
   ├─> SegmentExpr 读取 city 字段数据
   ├─> CompareExpr 执行 = "Beijing" 比较
   └─> LogicalBinaryExpr 执行 AND 操作
   
4. 生成过滤位图
   └─> 传递给 VectorSearchNode
   
5. VectorSearchNode执行
   └─> 使用位图进行向量搜索
   
6. 返回结果
```

### 6.2 数据流转

```
原始数据（Segment）
    ↓
表达式执行器读取和计算
    ↓
生成位图（哪些行满足条件）
    ↓
算子处理（过滤、搜索等）
    ↓
输出结果
```

## 7. 性能优化机制

### 7.1 批量执行
- 表达式支持批量处理，减少函数调用开销
- 使用 `batch_size` 控制批次大小

### 7.2 向量化执行
- 使用SIMD指令加速比较操作
- 批量内存访问优化

### 7.3 短路求值
- 逻辑表达式支持短路求值
- AND操作中，第一个条件为false时跳过第二个

### 7.4 索引利用
- SegmentExpr 优先使用索引
- 减少原始数据访问

## 8. 关键区别总结

| 特性 | 执行算子（Operator） | 表达式执行器（Expression） |
|------|---------------------|--------------------------|
| **职责** | 管理查询执行流程 | 执行具体计算 |
| **粒度** | 粗粒度（处理数据流） | 细粒度（处理单个表达式） |
| **输入** | RowVector（行向量） | 字段数据、常量值 |
| **输出** | RowVector（处理后的数据） | 布尔值或计算结果 |
| **状态** | 有状态（维护执行状态） | 无状态（纯计算） |
| **并发** | 支持并发执行 | 线程安全的计算 |
| **优化** | 流水线优化 | 向量化、批量执行 |

## 9. 设计优势

### 9.1 关注点分离
- 算子关注执行流程控制
- 表达式关注具体计算逻辑
- 清晰的职责划分

### 9.2 可扩展性
- 易于添加新算子
- 易于添加新表达式类型
- 通过组合实现复杂功能

### 9.3 性能优化
- 分层优化策略
- 算子级别的流水线优化
- 表达式级别的向量化优化

### 9.4 代码复用
- 表达式可在不同算子间复用
- 统一的表达式执行框架
- 减少重复代码

## 10. 实际应用场景

### 场景1：简单过滤查询
```
FilterBitsNode
  └─> CompareExpr (age > 18)
```

### 场景2：复杂条件查询
```
FilterBitsNode
  └─> LogicalBinaryExpr (AND)
      ├─> CompareExpr (age > 18)
      └─> TermExpr (city IN ["Beijing", "Shanghai"])
```

### 场景3：向量搜索with过滤
```
FilterBitsNode
  └─> CompareExpr (category = "electronics")
      ↓ 位图
VectorSearchNode
  └─> 使用位图进行向量搜索
```

## 总结

执行算子和表达式执行器是Milvus查询引擎的两个核心组件：

1. **执行算子**负责查询的整体执行流程，管理数据流转
2. **表达式执行器**负责具体的计算逻辑，返回计算结果
3. **ExprSet**作为桥梁，连接算子和表达式
4. 这种分层设计提供了良好的扩展性和性能优化空间

理解这两者的关系，对于深入理解Milvus的查询执行机制至关重要。