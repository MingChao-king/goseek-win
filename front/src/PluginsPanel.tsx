// 插件管理面板：查看、启用/禁用、卸载。
//
// 与 SkillsPanel 同构：.modal 体系、Esc 关闭、错误就地显示。
// 启停走 plugins.disabled 文件（不碰目录），卸载才删目录。

import { useCallback, useEffect, useState } from "react";
import { deletePlugin, listPlugins, togglePlugin, type PluginInfo } from "./api";
import { Icon } from "./Icon";

export function PluginsPanel({ onClose }: { onClose: () => void }) {
  const [plugins, setPlugins] = useState<PluginInfo[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const body = await listPlugins();
      setPlugins(body.plugins);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "读取插件列表失败");
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  async function handleToggle(name: string, enabled: boolean) {
    setBusy(name);
    try {
      await togglePlugin(name, enabled);
      await refresh();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "切换插件状态失败");
    } finally {
      setBusy(null);
    }
  }

  async function handleDelete(name: string) {
    setBusy(name);
    try {
      await deletePlugin(name);
      setExpanded(null);
      await refresh();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "卸载插件失败");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal skills-panel" onClick={(event) => event.stopPropagation()}>
        <header>
          <h2>Plugins</h2>
          <div className="modal-header-actions">
            <button type="button" className="icon-btn" onClick={onClose} title="关闭">
              <Icon name="close" size={14} />
            </button>
          </div>
        </header>
        {error && <p className="skills-error">{error}</p>}
        {plugins.length === 0 && !error && (
          <p className="skills-empty">
            还没有安装插件。把插件目录放到数据目录的 plugins/ 下，或使用内置插件。
          </p>
        )}
        <div className="skills-list">
          {plugins.map((p) => (
            <div key={p.name} className={`skill-item${expanded === p.name ? " expanded" : ""}`}>
              <div className="plugin-item-row">
                <button
                  type="button"
                  className="skill-item-header plugin-item-header"
                  onClick={() => setExpanded(expanded === p.name ? null : p.name)}
                >
                  <div className="skill-name-row">
                    <span className="skill-name">{p.display_name || p.name}</span>
                    {p.version && <span className="plugin-version">v{p.version}</span>}
                    {!p.enabled && <span className="plugin-disabled-chip">已禁用</span>}
                  </div>
                  <span className="skill-description">{p.description}</span>
                </button>
                <label className="plugin-toggle" title={p.enabled ? "禁用" : "启用"}>
                  <input
                    type="checkbox"
                    checked={p.enabled}
                    disabled={busy === p.name}
                    onChange={(event) => void handleToggle(p.name, event.target.checked)}
                  />
                  <span className="toggle-track" aria-hidden="true" />
                </label>
              </div>
              {expanded === p.name && (
                <div className="plugin-detail">
                  {p.author && <p className="plugin-meta">作者：{p.author}</p>}
                  {(p.skills ?? []).length > 0 && (
                    <p className="plugin-meta">Skills：{p.skills.join("、")}</p>
                  )}
                  {(p.commands ?? []).length > 0 && (
                    <p className="plugin-meta">命令：{p.commands.map((c) => `/${c}`).join(" ")}</p>
                  )}
                  {p.has_bin && <p className="plugin-meta">包含可执行工具（已注入命令环境）</p>}
                  <div className="skill-item-actions">
                    <button
                      type="button"
                      className="btn-danger"
                      disabled={busy === p.name}
                      onClick={() => void handleDelete(p.name)}
                    >
                      卸载
                    </button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
