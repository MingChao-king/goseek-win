// Skill 管理面板：查看、导入、删除 skill。
//
// 用户不应该手动管理文件——这是 V1.0 的明确要求。面板提供三个操作：
//   列表：启动时拉取，点条目展开正文；
//   导入：文件选择器读本地 .md 文件，名字取文件名去扩展名；
//   删除：每条目上的按钮，直接删除，不弹确认（本机个人工具，误删可重导）。

import { useCallback, useEffect, useRef, useState } from "react";
import { deleteSkill, getSkill, listSkills, uploadSkill, type SkillSummary } from "./api";
import { Icon } from "./Icon";

export function SkillsPanel({ onClose }: { onClose: () => void }) {
  const [skills, setSkills] = useState<SkillSummary[]>([]);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [content, setContent] = useState<string>("");
  const [error, setError] = useState<string | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    try {
      const body = await listSkills();
      setSkills(body.skills);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "读取 skill 列表失败");
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Esc 关闭，和其他弹层一致。
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  async function toggle(name: string) {
    if (expanded === name) {
      setExpanded(null);
      return;
    }
    try {
      const detail = await getSkill(name);
      setContent(detail.content);
      setExpanded(name);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "读取 skill 失败");
    }
  }

  async function handleImport(files: FileList | null) {
    if (!files) return;
    for (const file of Array.from(files)) {
      const name = file.name.replace(/\.md$/i, "").replace(/[^a-zA-Z0-9_-]/g, "-");
      const text = await file.text();
      try {
        await uploadSkill(name, text);
      } catch (failure) {
        setError(failure instanceof Error ? failure.message : `导入 ${file.name} 失败`);
      }
    }
    await refresh();
  }

  async function handleDelete(name: string) {
    try {
      await deleteSkill(name);
      if (expanded === name) setExpanded(null);
      await refresh();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "删除 skill 失败");
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal skills-panel" onClick={(event) => event.stopPropagation()}>
        <header>
          <h2>Skills</h2>
          <div className="modal-header-actions">
            <button type="button" className="icon-btn" onClick={() => fileInput.current?.click()} title="导入 skill 文件（.md）">
              <Icon name="upload" size={14} />
            </button>
            <button type="button" className="icon-btn" onClick={onClose} title="关闭">
              <Icon name="close" size={14} />
            </button>
          </div>
        </header>
        <input
          ref={fileInput}
          type="file"
          accept=".md,.markdown,text/markdown"
          multiple
          hidden
          onChange={(event) => {
            void handleImport(event.target.files);
            event.target.value = "";
          }}
        />
        {error && <p className="skills-error">{error}</p>}
        {skills.length === 0 && !error && (
          <p className="skills-empty">还没有 skill。点右上角的导入按钮选择 .md 文件，或安装插件获得内置 skill。</p>
        )}
        <div className="skills-list">
          {skills.map((skill) => (
            <div key={skill.name} className={`skill-item${expanded === skill.name ? " expanded" : ""}`}>
              <button type="button" className="skill-item-header" onClick={() => void toggle(skill.name)}>
                <div className="skill-name-row">
                  <span className="skill-name">{skill.name}</span>
                  {skill.source !== "user" && (
                    <span className="skill-source-chip">{skill.source}</span>
                  )}
                </div>
                <span className="skill-description">{skill.description}</span>
              </button>
              {expanded === skill.name && (
                <>
                  <pre className="skill-content">{content}</pre>
                  {skill.source === "user" ? (
                    <div className="skill-item-actions">
                      <button type="button" className="btn-danger" onClick={() => void handleDelete(skill.name)}>
                        删除
                      </button>
                    </div>
                  ) : (
                    <div className="skill-item-actions skill-item-readonly">
                      来自插件 {skill.source}，在插件面板中管理
                    </div>
                  )}
                </>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
