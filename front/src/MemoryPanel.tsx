// 摘要树面板：像模型一样逐层展开，并且可以就地修订活跃摘要。
//
// # 和模型看到的是同一棵树
//
// 模型用 conversation_history 的 inspect 一层层往下走，这里照着同样的形状展示。
// 区别只在人可以同时看到好几层的轮廓，而模型一次只能拿一层——那是有意的，
// 一口气展开整棵树等于把历史重新灌回它的上下文。
//
// **展开一次只多一层**，和模型的约束一致：一次全展开，人也一样会被淹没。
//
// # 原始消息不需要额外请求
//
// 快照里已经有完整历史，节点的 [start_message, end_message] 直接就是它在那个数组
// 里的下标区间。所以这里显示的原文，和模型 read 出来的是同一批消息。
//
// # 修订
//
// 只有**活跃前沿**上的节点可以改：改一个已经被合并进上层的节点，模型看到的东西
// 一个字都不会变，让它可编辑等于给用户一个假的反馈。后端也校验这一点。
//
// 改的不是模型原文——后端把两份都留着（content 永不修改，修订存在另一列），
// 因此改过的节点这里能展开对照"模型当初写的是什么"。

import { useMemo, useState } from "react";
import { parseSections, serializeSections } from "./sections";
import { Icon } from "./Icon";
import type { MemoryNode, MemoryTree, Message } from "./types";

export function MemoryPanel({
  tree, messages, activeIDs, busy, onEdit, onClose,
}: {
  tree: MemoryTree | null;
  messages: Message[];
  /** 活跃前沿的 ID 集合，决定哪些节点可以改。 */
  activeIDs: Set<string>;
  /** 会话里有事在跑：此时后端会拒绝修订，所以先把入口禁掉。 */
  busy: boolean;
  /** 保存修订。content 为空串表示撤销修订。 */
  onEdit: (batchID: string, content: string) => Promise<void>;
  onClose: () => void;
}) {
  if (!tree) {
    return (
      <aside className="memory-panel">
        <PanelHead onClose={onClose} />
        <p className="empty">正在读取摘要树…</p>
      </aside>
    );
  }

  const byID = new Map(tree.batches.map((node) => [node.id, node]));
  const merged = tree.batches.length - tree.active_batch_ids.length;

  return (
    <aside className="memory-panel">
      <PanelHead onClose={onClose} />

      {tree.batches.length === 0 ? (
        <p className="empty">这个会话还没有被压缩过，没有摘要节点。</p>
      ) : (
        <>
          <p className="memory-note">
            前 {tree.raw_compaction_cursor - 1} 条消息已折叠成{" "}
            {tree.active_batch_ids.length} 段摘要
            {merged > 0 && `，另有 ${merged} 段已合并进上层`}。
            原始消息一条都没有丢。
          </p>
          {tree.active_batch_ids.map((id) => {
            const node = byID.get(id);
            // 前沿引用了树里没有的节点，只能是数据被外部改过。跳过一个节点，
            // 比让整个面板打不开要好。
            return node ? (
              <NodeView
                key={id}
                node={node}
                byID={byID}
                messages={messages}
                activeIDs={activeIDs}
                busy={busy}
                onEdit={onEdit}
                depth={0}
              />
            ) : null;
          })}
        </>
      )}
    </aside>
  );
}

function PanelHead({ onClose }: { onClose: () => void }) {
  return (
    <div className="memory-panel-head">
      <strong>
        <Icon name="layers" size={15} /> 摘要树
      </strong>
      <button className="link" onClick={onClose}>
        收起
      </button>
    </div>
  );
}

/** NodeView 渲染一个节点：标题栏 + 按需展开的正文、子节点、原文、编辑框。 */
function NodeView({
  node, byID, messages, activeIDs, busy, onEdit, depth,
}: {
  node: MemoryNode;
  byID: Map<string, MemoryNode>;
  messages: Message[];
  activeIDs: Set<string>;
  busy: boolean;
  onEdit: (batchID: string, content: string) => Promise<void>;
  depth: number;
}) {
  // 四个开关各自独立：看正文不等于要看子节点，看子节点也不等于要看原文。
  const [openSummary, setOpenSummary] = useState(depth === 0);
  const [openChildren, setOpenChildren] = useState(false);
  const [openRaw, setOpenRaw] = useState(false);
  const [openOriginal, setOpenOriginal] = useState(false);
  // editing 是**正在编辑哪一节**：null 表示没在编辑，"" 表示在编辑整段（未分节
   // 时的降级路径），别的值是节名。
   //
   // 逐节编辑而不是整段：一段摘要有七节、上千字，改"未完成 / 阻塞"时在一大团
   // 文字里找那一段本身就是负担，而且很容易顺手改坏别的节。
  const [editing, setEditing] = useState<string | null>(null);

  const isLeaf = node.source_batch_ids.length === 0;
  const canEdit = activeIDs.has(node.id);
  // 下标区间：闭区间的序号转成数组下标。
  const covered = messages.slice(node.start_message - 1, node.end_message);

  return (
    <div className="memory-node" style={{ marginLeft: `${depth * 0.9}rem` }}>
      <div className="memory-node-head">
        <button
          className="memory-toggle"
          onClick={() => setOpenSummary((open) => !open)}
          title={openSummary ? "收起摘要" : "展开摘要"}
        >
          <Icon name={openSummary ? "chevronDown" : "chevronRight"} size={13} />
        </button>
        <span className="memory-level" data-depth={node.level}>
          L{node.level}
        </span>
        <span className="memory-range">
          #{node.start_message}–{node.end_message}
        </span>
        <span className="memory-title">{node.title}</span>
        {node.edited && <span className="memory-edited">已修订</span>}
      </div>

      {openSummary && (
        <div className="memory-node-body">
          <Prose
            content={node.content}
            original={node.original_content ?? ""}
            canEdit={canEdit && !busy}
            editing={editing}
            onEditing={setEditing}
            onSave={async (content) => {
              await onEdit(node.id, content);
              setEditing(null);
            }}
          />

          {editing === null && (
            <div className="memory-actions">
              {/* 一次只展开一层，和模型 inspect 的约束一致。 */}
              {!isLeaf && (
                <button className="link" onClick={() => setOpenChildren((open) => !open)}>
                  {openChildren ? "收起子节点" : `展开子节点（${node.source_batch_ids.length}）`}
                </button>
              )}
              {isLeaf && <span className="memory-note">叶子节点，直接覆盖原始消息</span>}

              <button className="link" onClick={() => setOpenRaw((open) => !open)}>
                {openRaw ? "收起原文" : `原文（${covered.length} 条）`}
              </button>

              {node.edited && (
                <button className="link" onClick={() => setOpenOriginal((open) => !open)}>
                  {openOriginal ? "收起模型原版" : "对照模型原版"}
                </button>
              )}

              {/* 编辑入口下放到每一节上（见 Prose）。这里只解释为什么不能编辑
                  ——说清楚原因，而不是把按钮藏起来让人猜。 */}
              {!canEdit && (
                <span className="memory-note">已并入上层，改它模型也看不到</span>
              )}
              {canEdit && busy && (
                <span className="memory-note">运行中，暂不能编辑</span>
              )}
            </div>
          )}

          {openOriginal && node.original_content && (
            <>
              <p className="memory-note">模型当初生成的版本（永不修改，仅供对照）：</p>
              <pre className="memory-content memory-original">{node.original_content}</pre>
            </>
          )}

          {openRaw && (
            <div className="memory-raw">
              {covered.map((message, index) => (
                <div key={index} className="memory-raw-item">
                  <span className="memory-range">
                    #{node.start_message + index} [{message.role}]
                  </span>
                  <pre>{message.content || "（无正文）"}</pre>
                </div>
              ))}
            </div>
          )}

          {openChildren &&
            node.source_batch_ids.map((childID) => {
              const child = byID.get(childID);
              return child ? (
                <NodeView
                  key={childID}
                  node={child}
                  byID={byID}
                  messages={messages}
                  activeIDs={activeIDs}
                  busy={busy}
                  onEdit={onEdit}
                  depth={depth + 1}
                />
              ) : null;
            })}
        </div>
      )}
    </div>
  );
}

/** Editor 是就地编辑摘要的文本框。 */
/**
 * Prose 按七节展示摘要正文，逐节可编辑。
 *
 * # 只有散文可编辑
 *
 * 一个摘要渲染进上下文时有三段：标题行、"期间用户说过"、七节散文。前两段是**渲染
 * 时从原始消息派生的**，不进存储，也就没有"编辑"这回事——它们本来就是原文，改它
 * 等于伪造历史。所以面板上只有散文这一段能改。这条划分让人工修订的语义变干净了：
 * 修订只碰模型写的东西。
 *
 * # 解析失败就整段展示
 *
 * 七节是提示词里的约定，不是协议。模型可能不按格式来，那时降级成一整块可编辑的
 * 文本——用户仍然能读、能改，只是没有分栏。不做机械校验：那是给模型能力打补丁，
 * 还会掩盖"这个模型不够用"这个真信号。
 */
function Prose({
  content, original, canEdit, editing, onEditing, onSave,
}: {
  content: string;
  /** 模型原始生成的那一版，用于"恢复模型原版"。没改过时是空串。 */
  original: string;
  canEdit: boolean;
  /** 正在编辑哪一节：null 没在编辑，"" 整段，其余是节名。 */
  editing: string | null;
  onEditing: (section: string | null) => void;
  onSave: (content: string) => Promise<void>;
}) {
  const sections = useMemo(() => parseSections(content), [content]);

  // 没分节：整段一个编辑框。
  if (sections.length === 0) {
    if (editing === "") {
      return (
        <Editor
          initial={content}
          original={original}
          onCancel={() => onEditing(null)}
          onSave={onSave}
        />
      );
    }
    return (
      <>
        <pre className="memory-content">{content}</pre>
        {canEdit && (
          <div className="memory-actions">
            <button className="link" onClick={() => onEditing("")}>编辑</button>
          </div>
        )}
      </>
    );
  }

  return (
    <div className="memory-sections">
      {sections.map((section) => (
        <div key={section.name} className="memory-section">
          <div className="memory-section-head">
            <span className="memory-section-name">{section.name}</span>
            {canEdit && editing === null && (
              <button className="link" onClick={() => onEditing(section.name)}>
                编辑
              </button>
            )}
          </div>
          {editing === section.name ? (
            <Editor
              initial={section.body}
              onCancel={() => onEditing(null)}
              // 改一节，发出去的是**重新拼出来的全文**——后端的 PATCH 收的是整段
              // 正文。往返一致性由 sections.ts 的测试钉住。
              onSave={(body) =>
                onSave(serializeSections(
                  sections.map((each) =>
                    each.name === section.name ? { ...each, body } : each)))
              }
            />
          ) : (
            <pre className="memory-content">{section.body}</pre>
          )}
        </div>
      ))}
    </div>
  );
}

function Editor({
  initial, original, onSave, onCancel,
}: {
  initial: string;
  /** 有原版说明这段已经改过，此时多给一个"恢复模型原版"的入口。 */
  original?: string;
  onSave: (content: string) => Promise<void>;
  onCancel: () => void;
}) {
  const [text, setText] = useState(initial);
  const [saving, setSaving] = useState(false);

  async function save(content: string) {
    setSaving(true);
    try {
      await onSave(content);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="memory-editor">
      <textarea
        value={text}
        onChange={(changeEvent) => setText(changeEvent.target.value)}
        onKeyDown={(keyEvent) => {
          // Esc 取消编辑。**必须 stopPropagation**：详情页也监听 Esc 来关整个
          // 面板，不拦住的话，改了一半按 Esc 会连面板带改动一起消失。
          // 就近的那个可退出的东西优先——这是 Esc 在所有界面里的一贯含义。
          if (keyEvent.key === "Escape") {
            keyEvent.stopPropagation();
            if (!saving) onCancel();
            return;
          }
          // Cmd/Ctrl + Enter 保存。摘要正文是多行的，Enter 必须留给换行。
          if (keyEvent.key === "Enter" && (keyEvent.metaKey || keyEvent.ctrlKey)) {
            keyEvent.preventDefault();
            if (!saving && text.trim()) void save(text);
          }
        }}
        rows={Math.min(20, Math.max(6, text.split("\n").length + 1))}
        disabled={saving}
      />
      <div className="memory-actions">
        <button onClick={() => void save(text)} disabled={saving || !text.trim()}>
          {saving ? "保存中…" : "保存"}
        </button>
        <button className="link" onClick={onCancel} disabled={saving}>
          取消
        </button>
        {original && (
          // 空串在后端就是"撤销修订"的意思，不需要另一个端点。
          <button className="link" onClick={() => void save("")} disabled={saving}>
            恢复模型原版
          </button>
        )}
      </div>
      <p className="memory-note">
        保存后这段摘要会以修订版进入下一次请求的上下文。模型原始生成的版本仍然留在库里。
      </p>
    </div>
  );
}
