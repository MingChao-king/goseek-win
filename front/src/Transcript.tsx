// 对话流的渲染。
//
// 按正常对话界面的习惯排布：用户消息右对齐气泡，助手正文左对齐不套气泡（正文是
// 主体，套气泡反而降低可读性），思考过程默认折叠，工具调用是一张卡片。
//
// **上下文占用不出现在这里的任何地方**——它不是对话的一部分，它在输入框下面的
// 仪表盘上（见 ContextGauge）。
//
// # 只有助手正文渲染 Markdown
//
//   用户消息      不渲染。他敲了 ** 就是想说 **，替他改成粗体是擅自解释输入
//   思考过程      不渲染。那是模型的原始草稿，忠实展示比好看重要
//   工具参数/输出  不渲染。命令输出必须逐字，一个字符都不能重排
//   摘要正文      不渲染。压缩指令里明确要求"不要使用 Markdown 标题"

import { memo, useEffect, useState } from "react";
import { CopyButton } from "./CopyButton";
import { Icon } from "./Icon";
import { ImageLightbox } from "./ImageLightbox";
import type { MessageImage } from "./types";
import { describeGroup, groupTurnItems, summarizeGroup } from "./grouping";
import { MarkdownView } from "./MarkdownView";
import type { CompactionStep, ToolStep, Turn, TurnItem } from "./reducer";

/** 命令输出默认展示的行数上限。 */
const maxOutputLines = 12;

export function Transcript({
  turns, foldedUpTo, onFileLinkClick,
}: {
  turns: Turn[];
  /** 折叠边界：前多少条消息模型看到的是摘要。0 表示一条都没折叠。 */
  foldedUpTo: number;
  /** file:// 链接点击 → 侧栏查看文件。 */
  onFileLinkClick?: (path: string) => void;
}) {
  if (turns.length === 0) {
    return <p className="empty">还没有消息。说点什么开始，或者输入 /help 看命令。</p>;
  }

  return (
    <div className="transcript">
      {turns.map((turn, index) => (
        <TurnView
          key={turn.id}
          turn={turn}
          index={index}
          nextEndMessage={turns[index + 1]?.endMessage ?? 0}
          foldedUpTo={foldedUpTo}
          onFileLinkClick={onFileLinkClick}
        />
      ))}
    </div>
  );
}

/**
 * 每个历史轮次单独缓存。流式回复每来一个片段，只会替换当前轮次的对象引用；
 * 已完成轮次保持原引用，因此不会把几十轮 Markdown 和工具输出全部重新解析一遍。
 */
const TurnView = memo(function TurnView({
  turn, index, nextEndMessage, foldedUpTo, onFileLinkClick,
}: {
  turn: Turn;
  index: number;
  nextEndMessage: number;
  foldedUpTo: number;
  onFileLinkClick?: (path: string) => void;
}) {
  return (
    <div className="turn-shell" data-turn-id={turn.id}>
      <section className="turn" aria-label={`第 ${index + 1} 轮`}>
        {/* 连续的工具调用合成一个可折叠的盒子——一轮里可能有十几条命令，
            每条单独占一张卡片的话，用户要滚过一大片输出才能看到真正的回复。
            key 用组内第一个片段的下标：片段只会往后追加、不会重排。 */}
        {groupTurnItems(turn.items).map((node) =>
          node.kind === "tools" ? (
            <ToolGroup key={node.key} steps={node.steps} />
          ) : (
            <ItemView key={node.key} item={node.item} onFileLinkClick={onFileLinkClick} />
          ),
        )}
      </section>
      <FoldMarker
        turn={turn}
        nextEndMessage={nextEndMessage}
        foldedUpTo={foldedUpTo}
      />
    </div>
  );
});

/**
 * FoldMarker 在折叠边界处画一条线。
 *
 * 它出现在**最后一轮完全落在游标之前**的那一轮之后。用户不知道这条线在哪，就会把
 * 模型对早期细节的模糊回答当成模型变笨了，而实际上那些细节确实不在它眼前——原文
 * 还在库里，模型要用 conversation_history 才能取到。
 */
function FoldMarker({
  turn, nextEndMessage, foldedUpTo,
}: { turn: Turn; nextEndMessage: number; foldedUpTo: number }) {
  if (foldedUpTo <= 0 || turn.endMessage === 0) return null;
  // 这一轮结束时还在折叠区内，而下一轮已经出了折叠区（或者没有下一轮）。
  const insideFolded = turn.endMessage <= foldedUpTo;
  const nextOutside = nextEndMessage === 0 || nextEndMessage > foldedUpTo;
  if (!insideFolded || !nextOutside) return null;

  return (
    <div className="fold-marker">
      <span>以上 {foldedUpTo} 条已折叠为摘要，模型看到的是概括而不是原文</span>
    </div>
  );
}

/** ItemView 按片段类型分派渲染。 */
function ItemView({ item, onFileLinkClick }: { item: TurnItem; onFileLinkClick?: (path: string) => void }) {
  switch (item.kind) {
    case "user":
      // 图片和文字分行：flex 行里并排会把气泡挤成竖条。气泡只装文字，
      // 图片在气泡下方成右对齐网格，点开灯箱看大图。
      return (
        <>
          {item.text && (
            <div className="row row-user">
              {/* 复制按钮在气泡外侧、左边：右边是气泡的边，放进去要么挤文字、
                  要么盖住最后一行。平时透明，hover 才显形（见 .copy-slot）。 */}
              <span className="copy-slot">
                <CopyButton text={item.text} />
              </span>
              <div className="bubble-user">{item.text}</div>
            </div>
          )}
          {item.images && item.images.length > 0 && (
            <MessageImages images={item.images} />
          )}
        </>
      );

    case "reasoning":
      // 思考过程默认折叠：它对理解 Agent 在想什么很有用，但不是回复本身。
      return (
        <details className="reasoning">
          <summary>思考</summary>
          <pre>{item.text}</pre>
        </details>
      );

    case "assistant":
      // 只有助手正文走 Markdown 渲染。用户消息、思考过程、工具参数与输出都不走——
      // 理由见文件开头。
      return (
        <div className="assistant-text">
          <MarkdownView text={item.text} onFileLinkClick={onFileLinkClick} />
          {/* 复制的是 Markdown 源码而不是渲染结果：用户要拿去贴到别处，
              源码才带着结构。只有说完了才给按钮——正在流式输出时复制到的是半句。 */}
          {item.final && (
            <div className="message-tools">
              <CopyButton text={item.text} />
            </div>
          )}
        </div>
      );

    case "tool":
      return <ToolCard step={item.step} />;

    case "compaction":
      return <CompactionRow step={item.step} />;

    case "failed":
      return <div className="turn-failed">本轮失败：{item.reason}</div>;
  }
}

/**
 * ToolGroup 把一组连续的工具调用收进一个可折叠的盒子。
 *
 * # 展开规则
 *
 *   还在跑   → 自动展开。执行过程必须看得见，否则用户面对的是一个沉默的盒子
 *   跑完了   → 自动收起。命令是过程，正文才是用户要的东西
 *   手动点过 → 听用户的，不再自动开合
 *
 * 最后一条是关键：自动行为一旦覆盖掉用户的明确操作，界面就变得不可预测——
 * 他刚展开看输出，下一条命令跑完又把他收起来了。
 */
function ToolGroup({ steps }: { steps: ToolStep[] }) {
  const running = summarizeGroup(steps).running > 0;
  const [open, setOpen] = useState(running);
  // touched 记录用户有没有亲手开合过。用 state 而不是 ref：它只在用户点击时变一次，
  // 而那一次本来就要重新渲染。
  const [touched, setTouched] = useState(false);

  // 没被手动动过时，跟着运行状态走。依赖里带上 touched，是为了让"用户点了一下"
  // 这件事立刻生效——否则下一次 running 变化还会把他的选择覆盖掉。
  useEffect(() => {
    if (!touched) setOpen(running);
  }, [running, touched]);

  return (
    <div className={`tool-group${running ? " running" : ""}`}>
      <button
        className="tool-group-head"
        onClick={() => {
          setTouched(true);
          setOpen((wasOpen) => !wasOpen);
        }}
      >
        <span className="tool-group-caret">{open ? "▾" : "▸"}</span>
        <span className="tool-group-label">{describeGroup(steps)}</span>
        <span className="tool-spacer" />
        {/* 收起状态下也要能看出结局，否则用户得展开才知道有没有出错。 */}
        {!open && <GroupBadges steps={steps} />}
      </button>

      {open && (
        <div className="tool-group-body">
          {steps.map((step) => (
            <ToolCard key={step.call.id} step={step} />
          ))}
        </div>
      )}
    </div>
  );
}

/** GroupBadges 在折叠状态下概括这一组的结局。 */
function GroupBadges({ steps }: { steps: ToolStep[] }) {
  const state = summarizeGroup(steps);
  return (
    <>
      {state.failed > 0 && <span className="tool-status error">{state.failed} 失败</span>}
      {state.running > 0 && <span className="tool-status running">执行中</span>}
      {state.failed === 0 && state.running === 0 && (
        <span className="tool-status success">全部成功</span>
      )}
    </>
  );
}

/** ToolCard 渲染一次工具调用：意图、真正发出的参数、输出和结局。 */
function ToolCard({ step }: { step: ToolStep }) {
  const result = step.result;
  return (
    <div className="tool-card">
      <div className="tool-card-head">
        <span className="tool-name">{step.call.name}</span>
        {/* 标题来自模型自述，参数原文才是事实——两者并列展示，
            对不上的时候一眼能看出来。 */}
        {step.title && <span className="tool-title">{step.title}</span>}
        <span className="tool-spacer" />
        {/* 命令是最常被复制走去手动重跑的东西。复制的是命令本身，
            不是那行 JSON——JSON 贴进终端不能执行。 */}
        <span className="copy-slot">
          <CopyButton text={commandOf(step)} label="复制命令" />
        </span>
        {result ? (
          <span className={`tool-status ${result.status}`}>
            {result.status}
            {result.exit_code !== undefined && ` · exit ${result.exit_code}`}
          </span>
        ) : (
          <span className="tool-status running">执行中…</span>
        )}
      </div>

      <pre className="tool-args">{JSON.stringify(step.call.arguments)}</pre>

      {step.output && <Output text={step.output} />}

      {/* 被程序阻止的调用（未知工具、参数非法）没有输出流，说明只在观察里，
          此时把它显示出来，否则用户只看到一个光秃秃的 error。 */}
      {result && result.exit_code === undefined && result.content && (
        <Output text={result.content} />
      )}
      {result?.images && result.images.length > 0 && (
        <div className="tool-result-images">
          {result.images.map((raw) => {
            // 历史 ToolResult JSON 里字段是大写 ID（修复前落库），兼容两种取值。
            const image = raw as { id?: string; ID?: string };
            const imageId = image.id ?? image.ID;
            if (!imageId) return null;
            return (
              <img
                key={imageId}
                src={`/api/v1/images/${imageId}`}
                alt={`工具产出图片 ${imageId}`}
                className="tool-result-image"
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

/**
 * Output 显示一段命令输出，超长时折叠。
 *
 * 一条 `ls /usr/bin` 有上千行，原样铺开会把整个执行过程刷走。这里只展示前若干行，
 * 其余折进一个可展开的区域并写明还有多少——**模型收到的仍然是完整（已按字节设
 * 上限的）观察**，折叠只发生在展示层。终端那边用的是同一个上限。
 */
function Output({ text }: { text: string }) {
  const lines = text.replace(/\n+$/, "").split("\n");
  if (lines.length <= maxOutputLines) {
    return (
      <div className="output-wrap">
        <pre className="tool-output">{text}</pre>
        <span className="copy-slot">
          <CopyButton text={text} label="复制输出" />
        </span>
      </div>
    );
  }

  const head = lines.slice(0, maxOutputLines).join("\n");
  const rest = lines.slice(maxOutputLines);
  return (
    <div className="output-wrap">
      <pre className="tool-output">{head}</pre>
      {/* 复制的是**完整** text 而不是屏幕上这十二行：折叠是展示层的事，
          用户点"复制输出"要的是全部，不是被我们裁过的一段。 */}
      <span className="copy-slot">
        <CopyButton text={text} label="复制输出" />
      </span>
      <details className="more-output">
        <summary>还有 {rest.length} 行（模型收到的是完整内容）</summary>
        <pre className="tool-output">{rest.join("\n")}</pre>
      </details>
    </div>
  );
}

/**
 * commandOf 从一次工具调用里取出可以直接贴进终端的命令。
 *
 * arguments 是模型给的 JSON，约定里 bash 工具的参数是 `{"command": "..."}`。
 * 取不到就退回参数原文——那至少还是事实，比复制到一个空串强。
 */
function commandOf(step: ToolStep): string {
  const args = step.call.arguments;
  if (args !== null && typeof args === "object" && "command" in args) {
    const command = (args as { command: unknown }).command;
    if (typeof command === "string") return command;
  }
  return JSON.stringify(step.call.arguments);
}

/**
 * CompactionRow 渲染发生在这一轮里的一次压缩。
 *
 * 它出现在用户提问和模型回答之间，也就是压缩真实发生的位置——这样用户能看出是哪次
 * 提问触发的，以及压缩前后差了多少。样式上和对话流区分开：它不是对话内容。
 */
function CompactionRow({ step }: { step: CompactionStep }) {
  const result = step.result;

  if (!result) {
    return (
      <div className="compaction running">
        正在整理上下文：已占用 {step.inputTokens.toLocaleString()} tokens，
        超过了 {step.threshold.toLocaleString()} 的触发线…
      </div>
    );
  }

  // 压缩失败不是本轮失败：这一轮照常带着未压缩的上下文继续，措辞上要把两件事分开，
  // 否则用户会以为回答也出问题了。
  if (result.failed) {
    return (
      <div className="compaction failed">
        整理上下文没有成功（{result.failed}），本轮仍按原上下文继续。
      </div>
    );
  }

  // 一个节点都没产生：什么都没压。说"已整理"是误导——那两个数字明明一样。
  if (result.batches.length === 0) {
    return (
      <div className="compaction">
        {result.target_unreachable
          ? `这次只整理了一部分，下一轮会继续（当前 ${result.after_tokens.toLocaleString()} tokens）`
          : `无需整理：当前 ${result.after_tokens.toLocaleString()} tokens，已在目标线以下`}
        {result.reason && <div className="compaction-reason">{result.reason}</div>}
      </div>
    );
  }

  const saved = result.before_tokens - result.after_tokens;
  return (
    <details className="compaction">
      <summary>
        已整理上下文：{result.before_tokens.toLocaleString()} →{" "}
        {result.after_tokens.toLocaleString()} tokens（省下 {saved.toLocaleString()}）
        {/* 没压到目标线不是失败，措辞要说清楚"下一轮会继续"，
            否则用户看到占用还很高会以为压缩坏了。 */}
        {result.target_unreachable && "，这次只整理了一部分，下一轮会继续"}
      </summary>
      {/* 原因单独一行，而且**在展开区的最上面**：走到收尾诊断时它是这条记录里
          最重要的信息——那不是正常工作状态，而是"该结束会话或窗口配小了"的信号。 */}
      {result.reason && <p className="compaction-reason">{result.reason}</p>}
      <ul className="memory-list">
        {result.batches.map((batch) => (
          <li key={batch.id}>
            <span className="memory-range">
              #{batch.start_message}–{batch.end_message}
            </span>
            {batch.level > 0 && <span className="memory-level">L{batch.level}</span>}
            <span className="memory-title">{batch.title}</span>
          </li>
        ))}
      </ul>
    </details>
  );
}

/**
 * MessageImages 渲染一则用户消息附带的图片：右对齐网格 + 点开灯箱。
 *
 * 缩略图是 cover 裁切、固定高度——它的职责是"认出是哪张图"，不是展示全部
 * 细节；细节交给灯箱。单图放宽到 380px，多图每张 260px，一行放得下两张。
 */
function MessageImages({ images }: { images: MessageImage[] }) {
  // null 表示灯箱关着；数字是当前看的第几张。
  const [viewing, setViewing] = useState<number | null>(null);

  return (
    <>
      <div className={`message-images${images.length === 1 ? " single" : ""}`}>
        {images.map((image, index) => (
          <button
            key={image.id}
            className="message-image-frame"
            title="点击放大"
            onClick={() => setViewing(index)}
          >
            <img
              src={`/api/v1/images/${image.id}`}
              alt=""
              className="message-image"
              loading="lazy"
            />
            <span className="message-image-zoom">
              <Icon name="expand" size={14} />
            </span>
          </button>
        ))}
      </div>
      {viewing !== null && (
        <ImageLightbox
          images={images}
          index={viewing}
          onClose={() => setViewing(null)}
          onNavigate={setViewing}
        />
      )}
    </>
  );
}
