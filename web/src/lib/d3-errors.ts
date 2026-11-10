// 把写作门禁的机器错误码翻译成业务用户能照着处理的话。纯函数，单测在 d3-errors.test.ts。

const messages: Record<string, string> = {
  storyboard_missing: '本章节还没有故事板。请先编写故事板并通过粉队评审，再写作或派发 AI 撰写。',
  storyboard_not_approved: '故事板尚未通过粉队评审，暂不能写作。请先提交评审，由另一位评审人批准。',
  storyboard_stale: '故事板依据的大纲、要求、证据或承诺已变化。请更新故事板后重新提交评审。',
  storyboard_state: '故事板状态已变化，请刷新后重试。',
  pink_self_approval: '粉队批准人不能是故事板的作者或提交人。请把“当前操作人”改为评审人后再批准。',
  red_self_approval: '定稿人不能是这一版正文的作者。请把“当前操作人”改为评审人后再批准定稿。',
  evidence_inadmissible: '引用的企业证据不可采或未列入故事板，请核对可采性结论和故事板引用。',
}

/** storyboard_invalid 的后端说明含字段名，这里按内容转成具体的填写提示。 */
function storyboardInvalid(message: string): string {
  if (/theme_sentence/.test(message)) return '核心论点至少需要 12 个有实际含义的字。'
  const missing = /missing mapped requirements:\s*(.+)$/.exec(message)
  if (missing) return `故事板没有覆盖这些要求：${missing[1]}。请为每条要求填写我方做法和采购人收益。`
  if (/our_approach and buyer_benefit/.test(message)) return '每条要点都要同时填写“我方做法”和“采购人收益”。'
  if (/at least one scoring_point/.test(message)) return '请至少填写一条写作要点。'
  return '故事板内容不完整，请检查后重试。'
}

/** commitment_unconfirmed 既来自确认承诺，也来自引用未确认的承诺，要按具体原因提示。 */
function commitmentUnconfirmed(message: string): string {
  if (/requires a tender requirement source/.test(message)) return '非价格承诺必须关联一条招标要求，请修改后再确认。'
  if (/requires unit, owner and rationale/.test(message)) return '确认前请补全单位、交付责任人和承诺说明。'
  if (/cannot be normalized/.test(message)) return '招标阈值或承诺值无法换算成数值，请填写数字。'
  if (/is rejected/.test(message)) return '这条承诺已被退回，不能使用。'
  return '引用的承诺未确认或未列入故事板，请先在“证据与承诺”确认，并加入故事板。'
}

/** ungrounded_claim 的后端说明列出了具体句子，保留下来，用户才知道要改哪里。 */
function afterColon(message: string, marker: RegExp): string {
  const m = marker.exec(message)
  return m ? message.slice(m.index + m[0].length).trim() : ''
}

/** d3ErrorMessage 返回业务文字；不是写作门禁的错误码时返回 undefined，由调用方按原样显示。 */
export function d3ErrorMessage(code: string | undefined, message: string): string | undefined {
  if (!code) return undefined
  if (code === 'storyboard_invalid') return storyboardInvalid(message)
  if (code === 'commitment_unconfirmed') return commitmentUnconfirmed(message)
  if (code === 'ungrounded_claim') {
    const detail = afterColon(message, /claim\(s\):/)
    return detail ? `写作核验未通过，以下内容没有对应证据或承诺：${detail}` : '写作核验未通过：正文中有没有证据支撑的断言。'
  }
  if (code === 'writing_verification_failed') {
    const detail = afterColon(message, /verification failed:/)
    return detail ? `写作核验未通过：${detail}` : '写作核验未通过，请运行写作核验查看原因。'
  }
  return messages[code]
}
