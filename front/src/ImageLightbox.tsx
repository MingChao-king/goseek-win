// 图片灯箱：点开全屏查看，支持左右翻页、Esc 关闭。
//
// 挂在对话流层级而不是单张图片上：同一则消息的几张图之间要能翻页，
// 状态（当前看第几张）放在父组件，这里只是受控渲染。
//
// 不做 pinch 缩放和拖拽平移：这是本机桌面工具，contain 到 92vw/88vh
// 已经能看清细节；做缩放就要处理手势冲突和边界回弹，复杂度不成比例。

import { useEffect } from "react";
import { Icon } from "./Icon";
import type { MessageImage } from "./types";

export function ImageLightbox({
  images, index, onClose, onNavigate,
}: {
  images: MessageImage[];
  /** 当前展示第几张。 */
  index: number;
  onClose: () => void;
  onNavigate: (index: number) => void;
}) {
  const image = images[index];
  const many = images.length > 1;

  // Esc 关闭，←/→ 翻页。挂在 window：打开灯箱时焦点可能在任何地方。
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
      if (!many) return;
      if (event.key === "ArrowLeft") onNavigate((index - 1 + images.length) % images.length);
      if (event.key === "ArrowRight") onNavigate((index + 1) % images.length);
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, images.length, many, onClose, onNavigate]);

  if (!image) return null;

  return (
    // 点遮罩关闭；点图片本身不关——用户可能在细看。
    <div className="lightbox-backdrop" onClick={onClose} role="dialog" aria-label="图片查看">
      <button className="lightbox-close" onClick={onClose} title="关闭（Esc）">
        <Icon name="close" size={18} />
      </button>

      {many && (
        <button
          className="lightbox-nav lightbox-prev"
          title="上一张（←）"
          onClick={(event) => {
            event.stopPropagation();
            onNavigate((index - 1 + images.length) % images.length);
          }}
        >
          <Icon name="chevronLeft" size={22} />
        </button>
      )}

      <img
        src={`/api/v1/images/${image.id}`}
        alt=""
        className="lightbox-image"
        onClick={(event) => event.stopPropagation()}
      />

      {many && (
        <button
          className="lightbox-nav lightbox-next"
          title="下一张（→）"
          onClick={(event) => {
            event.stopPropagation();
            onNavigate((index + 1) % images.length);
          }}
        >
          <Icon name="chevronRight" size={22} />
        </button>
      )}

      {many && (
        <span className="lightbox-counter">
          {index + 1} / {images.length}
        </span>
      )}
    </div>
  );
}
