// 内联 SVG 图标集。
//
// 为什么不引图标库：面板只用十几个图标，引库要么 tree-shake 之后还要拖一个包，
// 要么 copy 一堆 license 文件；手写的 SVG 只有基础笔画（圆 / 矩形 / 线），
// 精确、零依赖、和代码一起 review。
//
// 统一规格：24 视窗、stroke 1.8、圆角端点——视觉上是一套东西。
// 默认不描边填充，和文字同高同粗。

export function Icon({ name, size = 16 }: { name: IconName; size?: number }) {
  const path = paths[name];
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {path}
    </svg>
  );
}

export type IconName = keyof typeof paths;

const paths = {
  sparkles: (
    <>
      <path d="M12 3v4M12 17v4M3 12h4M17 12h4" />
      <path d="M12 8.5 13.2 12 16.5 13.2 13.2 14.4 12 18l-1.2-3.6L7.5 13.2 10.8 12z" />
    </>
  ),
  plus: <path d="M12 5v14M5 12h14" />,
  minus: <path d="M5 12h14" />,
  send: <path d="M5 12h13M13 6l6 6-6 6" />,
  stop: <rect x="7" y="7" width="10" height="10" rx="1.5" />,
  folder: (
    <path d="M3.5 6.5a1.5 1.5 0 0 1 1.5-1.5h4l2 2.5h8a1.5 1.5 0 0 1 1.5 1.5v9a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5z" />
  ),
  layers: (
    <>
      <path d="m12 3 9 5-9 5-9-5z" />
      <path d="m3 13 9 5 9-5" />
    </>
  ),
  list: <path d="M9 6h11M9 12h11M9 18h11M4.5 6h.01M4.5 12h.01M4.5 18h.01" />,
  clock: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M12 7.5V12l3 2" />
    </>
  ),
  alert: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M12 8v5M12 16.5h.01" />
    </>
  ),
  chevronDown: <path d="m6 9.5 6 6 6-6" />,
  chevronRight: <path d="m9.5 6 6 6-6 6" />,
  chevronLeft: <path d="m14.5 6-6 6 6 6" />,
  copy: (
    <>
      <rect x="9" y="9" width="11" height="11" rx="1.5" />
      <path d="M5 15V5.5A1.5 1.5 0 0 1 6.5 4H15" />
    </>
  ),
  check: <path d="m4.5 12.5 5 5L20 6.5" />,
  close: <path d="M6 6l12 12M18 6 6 18" />,
  more: (
    <>
      <circle cx="5.5" cy="12" r="0.4" fill="currentColor" />
      <circle cx="12" cy="12" r="0.4" fill="currentColor" />
      <circle cx="18.5" cy="12" r="0.4" fill="currentColor" />
    </>
  ),
  edit: (
    <>
      <path d="M12 20h8.5" />
      <path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z" />
    </>
  ),
  archive: (
    <>
      <rect x="3.5" y="4.5" width="17" height="4.5" rx="1" />
      <path d="M5 9v9.5A1.5 1.5 0 0 0 6.5 20h11a1.5 1.5 0 0 0 1.5-1.5V9M10 13.5h4" />
    </>
  ),
  trash: (
    <>
      <path d="M4 7h16M9.5 7V5a1 1 0 0 1 1-1h3a1 1 0 0 1 1 1v2M6.5 7l1 12a1.5 1.5 0 0 0 1.5 1.4h6A1.5 1.5 0 0 0 16.5 19l1-12" />
      <path d="M10 11v6M14 11v6" />
    </>
  ),
  compress: (
    <>
      <path d="M8 3.5v4a1 1 0 0 1-1 1H3M16 3.5v4a1 1 0 0 0 1 1h4M8 20.5v-4a1 1 0 0 0-1-1H3M16 20.5v-4a1 1 0 0 1 1-1h4" />
    </>
  ),
  command: (
    <path d="M9 9V6.5A2.5 2.5 0 1 0 6.5 9H9zm0 0v6m0-6h6m-6 6v2.5A2.5 2.5 0 1 1 6.5 15H9zm6 0h2.5a2.5 2.5 0 1 1-2.5 2.5V15zm0-6h2.5A2.5 2.5 0 1 0 15 6.5V9z" />
  ),
  refresh: (
    <>
      <path d="M20 12a8 8 0 1 1-2.3-5.6" />
      <path d="M20 3.5V8h-4.5" />
    </>
  ),
  upload: <path d="M12 15V4M8 8l4-4 4 4M5 15v3a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-3" />,
  arrowDown: <path d="M12 4.5v15M6 13.5l6 6 6-6" />,
  puzzle: (
    <path d="M9 4.5h2.2a1.4 1.4 0 0 1 2.8 0H16a1.5 1.5 0 0 1 1.5 1.5v2.3a1.4 1.4 0 0 1 0 2.8V16a1.5 1.5 0 0 1-1.5 1.5h-2.2a1.4 1.4 0 0 0-2.8 0H8A1.5 1.5 0 0 1 6.5 16v-2.2a1.4 1.4 0 0 0-2.8 0V9.8A1.5 1.5 0 0 1 5.2 8.3H7.5V6A1.5 1.5 0 0 1 9 4.5z" />
  ),
  pulse: (
    <path d="M3 12h4l3-8 4 16 3-8h4" stroke="currentColor" strokeWidth="2" fill="none" strokeLinecap="round" strokeLinejoin="round"/>
  ),
  globe: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M3.5 12h17M12 3.5c2.6 2.3 4 5.3 4 8.5s-1.4 6.2-4 8.5c-2.6-2.3-4-5.3-4-8.5s1.4-6.2 4-8.5z" />
    </>
  ),
  file: (
    <path d="M6 3.5h7l5 5V20a1.5 1.5 0 0 1-1.5 1.5h-10A1.5 1.5 0 0 1 5 20V5a1.5 1.5 0 0 1 1-1.5zM13 3.5V9h5" />
  ),
  expand: (
    <>
      <path d="M14 5h5v5M19 5l-7 7M10 19H5v-5M5 19l7-7" />
    </>
  ),
};
