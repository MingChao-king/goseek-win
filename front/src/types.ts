// 本文件是后端 HTTP 契约在前端的镜像。
//
// 它和 Go 那边的 internal/httpapi/dto.go 一一对应。TypeScript 的类型只在编译期
// 存在，运行时不会校验任何东西——所以这里写的是"我们相信后端会返回什么"，
// 一旦后端改了字段名，这里必须跟着改，否则编译通过但运行时拿到 undefined。

/** 会话列表里的一项。 */
/** 一个正在运行的会话：侧栏"正在运行"提醒的数据单元。 */
export interface RunningSession {
  id: string;
  /** 当前轮进行到的步骤（思考中 / 执行中 / 整理上下文）。 */
  state: RunState;
}

export interface SessionSummary {
  id: string;
  title: string;
  message_count: number;
  /** RFC3339 时间字符串，用 new Date() 解析。 */
  updated_at: string;
  workspace: string;
  /** 该会话使用的模型；空串表示沿用服务启动默认模型。 */
  model: string;
  /** 被归档了：列表默认不显示它。归档不动任何数据，只影响"要不要出现在眼前"。 */
  archived: boolean;
  /** 标题是用户自己起的，不是从首条消息派生的。 */
  custom_title: boolean;
  /**
   * 这个会话此刻是否有轮在跑。null 表示不在跑。
   *
   * 它由侧栏定时轮询 /api/v1/sessions/running 合并进来（服务端不把它放进列表
   * 响应：运行状态瞬息万变，让它跟着列表的 10 秒刷新走会显得迟钝）。侧栏用它
   * 画"正在运行"的提醒——多会话并行时，哪个会话模型还在处理必须一眼可见。
   */
  running_state: RunState | null;
}

/** 一次工具调用。 */
export interface ToolCall {
  id: string;
  name: string;
  /** 参数已经是解析好的对象，不需要再 JSON.parse。 */
  arguments: unknown;
}

/** 会话历史里的一条消息。 */
export interface Message {
  role: "user" | "assistant" | "tool";
  content: string;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
  /** 同一轮交互里的消息共享这个值，界面据此分组。 */
  turn_id: string;
  /** 用户消息附带的图片，按发送顺序。 */
  images?: MessageImage[];
}

/** MessageImage 是消息附带的一张图片引用。 */
export interface MessageImage {
  id: string;
  media_type: string;
  width: number;
  height: number;
}

/** 会话快照：完整历史加上当前的事件序号。 */
export interface SessionSnapshot {
  id: string;
  workspace: string;
  created_at: string;
  updated_at: string;
  /**
   * 这个会话当前的最大事件序号。
   *
   * 它是衔接实时事件流的锚点：先用快照渲染完整历史，再从这个序号往后订阅，
   * 中间不重不漏。没有它的话，"取快照"和"开始订阅"之间产生的事件要么漏掉
   * 要么重复。
   */
  last_sequence: number;
  messages: Message[];
  /**
   * 快照瞬间的运行状态；缺省（旧后端）按空闲处理。
   *
   * 思考阶段（reasoning）不产生 state.changed 事件，事件流重放救不了它——
   * "思考中切走再切回显示空闲"就出在这里，所以它必须由快照显式携带。
   */
  run_state?: RunState;
  /**
   * 这个会话的压缩现状。
   *
   * 它必须来自快照：压缩事件是历史事件，而事件流只从 last_sequence 之后订阅，
   * 刷新页面时不会重放到它们。没有这个字段，刷新之后界面就会以为"这个会话
   * 从没压缩过"，然后把一段其实已经被折叠的历史当作完整历史展示。
   */
  memory: SessionMemory;
  /**
   * 这个会话**下一次请求**会占多少上下文。
   *
   * 它必须来自快照，理由和 memory 一样：面板刚打开时还没有下一次请求，事件流也
   * 只从 last_sequence 之后订阅，看不到历史上的 usage 事件。没有这个字段，
   * 刷新页面之后仪表盘就是空的，直到用户再发一条消息——那看起来像是功能没了。
   */
  usage: ContextUsage;
  /** 当前生效的模型名。 */
  model: string;
  /** 模型窗口信息；后端算好，前端不复制阈值公式。 */
  model_info: ModelInfo;
}

/** 模型目录里的一项。 */
export interface ModelInfo {
  name: string;
  display_name: string;
  context_window: number;
  effective_context_window: number;
  compaction_trigger: number;
  window_source: string;
  measured_at: string;
}

/**
 * 一个摘要节点在界面上的形态。
 *
 * 快照里的活跃前沿和压缩事件里新生成的节点用的是**同一个**形状，因此可以用
 * 同一个组件渲染。后端刻意保证了这一点（见 dto.go 的 memoryBatchView）。
 */
export interface MemoryBatch {
  id: string;
  /** 层级：直接由原始消息生成的叶子是 0，每合并一次加一。 */
  level: number;
  /** 摘要首行的概括。 */
  title: string;
  /** 覆盖的消息序号，从 1 开始的闭区间——是给人看的，不是数组下标。 */
  start_message: number;
  end_message: number;
}

/** 会话的压缩现状。 */
export interface SessionMemory {
  /** 仍以原文进入上下文的第一条消息序号（从 1 开始）。1 表示一条都没折叠。 */
  raw_compaction_cursor: number;
  /** 当前真正进入上下文的那一层摘要。 */
  active_batches: MemoryBatch[];
  /** 仓库里的节点总数，含已被合并进上层、不再直接生效的那些。 */
  total_batches: number;
}

/** 事件类型。取值与后端 domain.EventType 一致。 */
export type EventType =
  | "turn.started"
  | "state.changed"
  | "user.message"
  | "assistant.delta"
  | "assistant.reasoning.delta"
  | "assistant.message"
  | "tool.started"
  | "tool.output.delta"
  | "tool.resolved"
  | "file.changed"
  | "turn.completed"
  | "turn.failed"
  | "context.usage.updated"
  | "context.compaction.started"
  | "context.compaction.completed";

/**
 * Agent 的运行状态。
 *
 * COMPRESSING 是"正在整理上下文"：它可能持续几十秒（每生成一个摘要节点都是
 * 一次模型调用），界面必须把它和 WAITING_MODEL 区分开——否则用户看到的是
 * 一个异常漫长的"等待模型"，会以为卡住了。
 */
export type RunState =
  | "IDLE"
  | "WAITING_MODEL"
  | "RUNNING_TOOL"
  | "COMPRESSING"
  | "FAILED";

/** 一个运行事件。 */
export interface RunEvent {
  /** durable 事件的序号；transient 事件（各种 delta）恒为 0。 */
  sequence: number;
  turn_id: string;
  type: EventType;
  /**
   * 事件内容，形状由 type 决定。
   *
   * 声明成 unknown 而不是 any：unknown 强制你在使用前先收窄类型，any 则会
   * 让后面所有的属性访问都失去检查。收窄工作交给 payload.ts 里的那组函数。
   */
  payload: unknown;
  at: string;
}

/** 工具调用的观察结果。 */
export interface ToolResult {
  tool_call_id: string;
  name: string;
  status: "success" | "error";
  content: string;
  exit_code?: number;
  /** 工具产出的图片（如浏览器截图），按返回顺序。 */
  images?: { id: string; media_type: string; width: number; height: number }[];
}

/**
 * 上下文占用。
 *
 * `remaining` 和 `ratio` 是后端算好一并发过来的派生值——在 Go 那边它们是方法，
 * 而方法过不了 JSON。让前端自己重算，等于把"窗口未知时比例算 0""剩余不能为负"
 * 这些规则复制一份到消费端，两处迟早会不一致。
 */
export interface ContextUsage {
  /** 模型一次调用的总容量。0 表示用户没有配置窗口大小。 */
  context_window: number;
  input_tokens: number;
  remaining: number;
  /** 占用比例。可能大于 1——估算偏高时确实会这样，那正是该压缩的信号。 */
  ratio: number;
  /** 这个数字有多可信：估算的、供应商实测的，还是压根不知道。 */
  source: "unknown" | "estimated" | "provider";
  /**
   * 这次输入里命中/未命中供应商上下文缓存的部分，以及命中比例。
   *
   * 只在供应商报了的时候才有（后端用 omitempty，缺席就是 undefined）。**不要用
   * `?? 0` 把缺席当成 0**：0 命中是一个有意义的值——压缩刚换掉整条前缀时正好是 0，
   * 而那恰恰是最值得看的一次。
   *
   * 它不参与任何判断，纯观测：命中率崩掉意味着请求前缀里混进了会变的东西，
   * 那是一个只体现为"变慢变贵"的静默回归。
   */
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
  cache_hit_ratio?: number;
}

/**
 * 一次压缩的结果。
 *
 * 无论成功与否后端都会发这个事件——失败时 `failed` 非空。不发的话界面会永远
 * 停在"正在压缩"。
 */
export interface CompactionResult {
  /** 压缩前后的上下文占用估算。 */
  before_tokens: number;
  after_tokens: number;
  /** 本次新生成的摘要节点。 */
  batches: MemoryBatch[];
  /**
   * 已经低于硬边界、但没到目标线。
   *
   * 它**不是失败**：这一轮的上下文贵一些，下一轮压缩会继续。要显示出来，
   * 否则用户看到压完占用还很高，会以为压缩失败了。
   */
  target_unreachable: boolean;
  /**
   * 为什么没压到目标线，或者收尾诊断发现了什么。
   *
   * 收尾诊断（用户原话被整理、切进保留区）不是正常工作状态，而是一个信号：
   * 这个会话该结束了，或者窗口配小了。只说"没压到"等于没说。
   */
  reason: string;
  /** 非空表示压缩没有成功完成，内容是原因。压缩失败不影响本轮回答。 */
  failed: string;
}

/**
 * 完整的摘要树，`GET …/memory` 的响应体。
 *
 * 它和快照里那个 `SessionMemory` 的区别只有两点，但都很关键：**节点是全部的**
 * （含已被合并进上层、不再直接生效的那些），而且**带摘要正文**。前者是"折叠到
 * 哪儿了"，后者是"这棵树长什么样"——只在用户主动展开时才请求一次。
 */
export interface MemoryTree {
  raw_compaction_cursor: number;
  /** 当前生效的那一层，按覆盖时间排序。节点本身在 batches 里，这里只给 ID。 */
  active_batch_ids: string[];
  /** 全部节点，按创建顺序（父节点必然晚于它的子节点）。 */
  batches: MemoryNode[];
}

/** 摘要树里的一个节点。 */
export interface MemoryNode {
  id: string;
  level: number;
  title: string;
  /** **当前生效**的正文：有人工修订就是修订版，没有就是模型原文。 */
  content: string;
  /**
   * 模型当初生成的那一版。只在被修订过时出现。
   *
   * 后端两份都留着（`content` 列永不修改，修订存在另一列），界面因此能让人对照
   * "我改了什么"。这也是"不可变的是事实、可变的是取用"这条设计在前端的落点。
   */
  original_content?: string;
  /** 被人工修订过。等价于 original_content 非空，但由后端显式给出，不要自己推导。 */
  edited: boolean;
  /**
   * 覆盖的消息序号，从 1 开始的闭区间。
   *
   * 快照里已经有完整历史，因此这个区间直接就是原文在 `messages` 数组里的下标
   * 范围 `[start_message - 1, end_message)`——不需要"读取节点覆盖的原始消息"
   * 这样一个额外端点。
   */
  start_message: number;
  end_message: number;
  /** 直接子节点。空表示这是叶子。 */
  source_batch_ids: string[];
}

/** 后端返回的错误体。 */
export interface ApiError {
  error: {
    /** 稳定的错误标识，按它分支。 */
    code: string;
    /** 给人看的说明，不要用它做判断。 */
    message: string;
  };
}
