// MCP server 管理面板：查看、添加、编辑、删除。
//
// 配置整体保存（POST /api/v1/mcp），每个 server 是 name + command + args。
// 表单校验就地显示：name/command 必填，重复名拒绝。

import { useCallback, useEffect, useState } from "react";
import { deleteMCP, listMCP, saveMCP, type MCPServerConfig } from "./api";
import { Icon } from "./Icon";

export function MCPPanel({ onClose }: { onClose: () => void }) {
  const [servers, setServers] = useState<MCPServerConfig[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  /** editing 是当前正在编辑的 server 原名；空串表示没有在编辑。 */
  const [editing, setEditing] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editCommand, setEditCommand] = useState("");
  const [editArgs, setEditArgs] = useState("");
  const [newName, setNewName] = useState("");
  const [newCommand, setNewCommand] = useState("");
  const [newArgs, setNewArgs] = useState("");

  const refresh = useCallback(async () => {
    try {
      const body = await listMCP();
      setServers(body.servers);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "读取 MCP 配置失败");
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape" && !adding) onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, adding]);

  async function persist(next: MCPServerConfig[]) {
    try {
      const body = await saveMCP(next);
      setServers(body.servers);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "保存配置失败");
    }
  }

  function handleAdd() {
    const name = newName.trim();
    const command = newCommand.trim();
    if (!name || !command) {
      setError("名称和启动命令都是必填的");
      return;
    }
    if (servers.some((s) => s.name === name)) {
      setError(`名称 "${name}" 已存在`);
      return;
    }
    const args = newArgs.trim() ? newArgs.trim().split(/\s+/) : [];
    void persist([...servers, { name, command, args }]);
    setAdding(false);
    setNewName("");
    setNewCommand("");
    setNewArgs("");
  }

  async function handleDelete(name: string) {
    try {
      const body = await deleteMCP(name);
      setServers(body.servers);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "删除失败");
    }
  }

  function startEdit(server: MCPServerConfig) {
    setEditing(server.name);
    setEditName(server.name);
    setEditCommand(server.command);
    setEditArgs((server.args ?? []).join(" "));
    setError(null);
  }

  async function handleSaveEdit(originalName: string) {
    const name = editName.trim();
    const command = editCommand.trim();
    if (!name || !command) {
      setError("名称和启动命令都是必填的");
      return;
    }
    if (name !== originalName && servers.some((s) => s.name === name)) {
      setError(`名称 "${name}" 已存在`);
      return;
    }
    const args = editArgs.trim() ? editArgs.trim().split(/\s+/) : [];
    const next = servers.map((s) =>
      s.name === originalName ? { name, command, args } : s,
    );
    await persist(next);
    setEditing(null);
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal skills-panel" onClick={(event) => event.stopPropagation()}>
        <header>
          <h2>MCP Servers</h2>
          <div className="modal-header-actions">
            <button type="button" className="icon-btn" onClick={() => setAdding(true)} title="添加 server">
              <Icon name="plus" size={14} />
            </button>
            <button type="button" className="icon-btn" onClick={onClose} title="关闭">
              <Icon name="close" size={14} />
            </button>
          </div>
        </header>
        {error && <p className="skills-error">{error}</p>}
        {adding && (
          <div className="mcp-add-form">
            <input
              autoFocus
              className="mcp-input"
              placeholder="名称，如 github"
              value={newName}
              onChange={(event) => setNewName(event.target.value)}
            />
            <input
              className="mcp-input"
              placeholder="启动命令，如 npx"
              value={newCommand}
              onChange={(event) => setNewCommand(event.target.value)}
            />
            <input
              className="mcp-input"
              placeholder="参数（空格分隔），如 -y @modelcontextprotocol/server-github"
              value={newArgs}
              onChange={(event) => setNewArgs(event.target.value)}
            />
            <div className="mcp-add-actions">
              <button type="button" className="btn-primary" onClick={handleAdd}>
                添加
              </button>
              <button
                type="button"
                className="btn-secondary"
                onClick={() => {
                  setAdding(false);
                  setError(null);
                }}
              >
                取消
              </button>
            </div>
          </div>
        )}
        {servers.length === 0 && !error && !adding && (
          <p className="skills-empty">
            还没有配置 MCP server。点右上角的 + 添加一个，重启 GoSeek 后生效。
          </p>
        )}
        <div className="skills-list">
          {servers.map((server) => (
            <div key={server.name} className="skill-item">
              {editing === server.name ? (
                <div className="mcp-add-form mcp-edit-form">
                  <input
                    autoFocus
                    className="mcp-input"
                    placeholder="名称"
                    value={editName}
                    onChange={(event) => setEditName(event.target.value)}
                  />
                  <input
                    className="mcp-input"
                    placeholder="启动命令"
                    value={editCommand}
                    onChange={(event) => setEditCommand(event.target.value)}
                  />
                  <input
                    className="mcp-input"
                    placeholder="参数（空格分隔）"
                    value={editArgs}
                    onChange={(event) => setEditArgs(event.target.value)}
                  />
                  <div className="mcp-add-actions">
                    <button type="button" className="btn-primary" onClick={() => void handleSaveEdit(server.name)}>
                      保存
                    </button>
                    <button type="button" className="btn-secondary" onClick={() => setEditing(null)}>
                      取消
                    </button>
                  </div>
                </div>
              ) : (
                <div className="plugin-item-row">
                  <button
                    type="button"
                    className="skill-item-header mcp-server-info"
                    title="点击编辑"
                    onClick={() => startEdit(server)}
                  >
                    <div className="skill-name-row">
                      <span className="skill-name">{server.name}</span>
                    </div>
                    <span className="skill-description mcp-command">
                      {server.command} {(server.args ?? []).join(" ")}
                    </span>
                  </button>
                  <button
                    type="button"
                    className="icon-btn"
                    title="删除此 server"
                    onClick={() => void handleDelete(server.name)}
                  >
                    <Icon name="trash" size={13} />
                  </button>
                </div>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
