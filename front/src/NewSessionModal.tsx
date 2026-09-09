// 新建会话弹窗。
//
// 主流程是系统文件夹选择器：用户授权后浏览器才能选择本机目录。手输路径是兼容
// 入口，只在 File System Access API 不可用时展开；它不是默认交互。

import { useEffect, useMemo, useState } from "react";
import { createSession, getDefaultWorkspace } from "./api";
import { Icon } from "./Icon";
import type { ModelInfo, SessionSummary } from "./types";

interface DirectoryPickerWindow extends Window {
  showDirectoryPicker?: (options?: { mode?: "read" | "readwrite" }) => Promise<unknown>;
}

export function NewSessionModal({
  sessions, models, onClose, onCreated,
}: {
  sessions: SessionSummary[];
  models: ModelInfo[];
  onClose: () => void;
  onCreated: (sessionID: string) => void;
}) {
  const [workspace, setWorkspace] = useState("");
  const [model, setModel] = useState("");
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const supported = useMemo(
    () => typeof (window as DirectoryPickerWindow).showDirectoryPicker === "function",
    [],
  );
  const recent = useMemo(() => {
    const byWorkspace = new Map<string, SessionSummary>();
    for (const session of sessions) {
      if (!byWorkspace.has(session.workspace)) byWorkspace.set(session.workspace, session);
    }
    return [...byWorkspace.values()]
      .sort((a, b) => b.updated_at.localeCompare(a.updated_at))
      .slice(0, 8);
  }, [sessions]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const body = await getDefaultWorkspace();
        if (!cancelled) setWorkspace(body.path);
      } catch {
        if (!cancelled) setWorkspace("");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  async function chooseFolder() {
    const pickerWindow = window as DirectoryPickerWindow;
    if (!pickerWindow.showDirectoryPicker) return;
    try {
      await pickerWindow.showDirectoryPicker({ mode: "read" });
      // Chromium 目前不把 handle.name 以外的绝对路径直接暴露给网页。
      // 真实可选路径要靠用户授权目录 + 手输校正；这里把目录名显示出来，
      // 并提示用户确认完整路径。
      setError("浏览器安全限制只能返回目录名，请确认或补全完整路径。");
    } catch (failure) {
      if (failure instanceof DOMException && failure.name === "AbortError") return;
      setError("文件夹选择失败，请手动输入路径。");
    }
  }

  async function create() {
    const path = workspace.trim();
    if (!path) {
      setError("请选择或输入工作区路径。");
      return;
    }
    setCreating(true);
    try {
      const created = await createSession(path, model);
      onCreated(created.id);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "新建会话失败");
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(event) => event.stopPropagation()}>
        <header>
          <h2>新建会话</h2>
          <button onClick={onClose} title="关闭">✕</button>
        </header>

        <label className="field">
          <span>工作区</span>
          <div className="workspace-row">
            <input
              value={workspace}
              placeholder="/absolute/path"
              onChange={(event) => setWorkspace(event.target.value)}
            />
            <button type="button" onClick={() => void chooseFolder()} disabled={!supported}>
              <Icon name="folder" size={14} />
              选择文件夹
            </button>
          </div>
        </label>

        {!supported && (
          <p className="field-note">当前浏览器不支持文件夹选择器，请手动输入路径。</p>
        )}

        {recent.length > 0 && (
          <div className="recent-workspaces">
            <span>最近使用</span>
            <ul>
              {recent.map((session) => (
                <li key={session.id}>
                  <button
                    type="button"
                    className={workspace === session.workspace ? "active" : ""}
                    onClick={() => setWorkspace(session.workspace)}
                    title={session.workspace}
                  >
                    {session.workspace}
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}

        <label className="field">
          <span>模型</span>
          <select value={model} onChange={(event) => setModel(event.target.value)}>
            <option value="">默认模型</option>
            {models.map((item) => (
              <option key={item.name} value={item.name}>
                {item.display_name}
              </option>
            ))}
          </select>
        </label>

        {error && <p className="error">{error}</p>}

        <footer>
          <button type="button" onClick={onClose}>取消</button>
          <button type="button" className="primary" onClick={() => void create()} disabled={creating}>
            {creating ? "创建中…" : "创建"}
          </button>
        </footer>
      </div>
    </div>
  );
}
