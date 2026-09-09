// 输入草稿的存取。
//
// 现在切个会话、刷新一下页面，打了一半的字就没了。这是纯粹的损失——用户没有
// 任何办法找回它，而它本来只需要存一行字符串。
//
// 按会话 id 分别存：切走再切回来，看到的应该是**这个会话**的草稿，而不是别的
// 会话的残留。
//
// 清空的时机只有一个：**发送成功之后**。发送失败、切换会话、关标签页都不清——
// 那些情况下用户还想要那段文字。

/** 键前缀。带上版本号，将来格式变了可以整批失效而不影响别的 localStorage 项。 */
const prefix = "goseek.draft.v1.";

/** loadDraft 取出某个会话的草稿。没有或读不出来时返回空串。 */
export function loadDraft(sessionID: string): string {
  if (!sessionID) return "";
  try {
    return window.localStorage.getItem(prefix + sessionID) ?? "";
  } catch {
    // localStorage 在隐私模式或配额满时会抛。草稿丢了是小事，
    // 让整个页面挂掉不是——所以这里吞掉。
    return "";
  }
}

/** saveDraft 存下某个会话的草稿。空串等同于清除，不留空项。 */
export function saveDraft(sessionID: string, text: string): void {
  if (!sessionID) return;
  try {
    if (text === "") {
      window.localStorage.removeItem(prefix + sessionID);
      return;
    }
    window.localStorage.setItem(prefix + sessionID, text);
  } catch {
    // 同上：存不下就算了，不能因此打断输入。
  }
}

/** clearDraft 清除某个会话的草稿。只在发送成功之后调用。 */
export function clearDraft(sessionID: string): void {
  saveDraft(sessionID, "");
}
