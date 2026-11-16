import assert from 'node:assert/strict'
import { test } from 'node:test'
import { d3ErrorMessage } from './d3-errors.ts'

test('reviewer separation asks for another signed-in reviewer', () => {
  assert.match(d3ErrorMessage('pink_self_approval', 'd3: x') ?? '', /另一位审核专家登录/)
  assert.doesNotMatch(d3ErrorMessage('pink_self_approval', 'd3: x') ?? '', /当前操作人/)
  assert.match(d3ErrorMessage('red_self_approval', 'd3: x') ?? '', /AI 代写的稿件算派发人/)
})

test('pre-upgrade declared names and the legacy policy explain the way out', () => {
  assert.match(d3ErrorMessage('legacy_actor', 'identity: x') ?? '', /映射到账号/)
  assert.match(d3ErrorMessage('quality_policy_legacy', 'identity: x') ?? '', /grounded\/v1/)
})

test('storyboard_invalid names what to fix, not the field', () => {
  const missing = d3ErrorMessage('storyboard_invalid', 'd3: invalid storyboard: storyboard scoring_points missing mapped requirements: R-1, R-2')
  assert.equal(missing, '故事板没有覆盖这些要求：R-1, R-2。请为每条要求填写我方做法和采购人收益。')
  assert.match(d3ErrorMessage('storyboard_invalid', 'd3: invalid storyboard: theme_sentence must have') ?? '', /核心论点/)
  assert.doesNotMatch(d3ErrorMessage('storyboard_invalid', 'something new') ?? '', /scoring_points/)
})

test('ungrounded claims keep the offending sentences', () => {
  const m = d3ErrorMessage('ungrounded_claim', 'd3: section revision contains ungrounded fact or commitment claims: 1 ungrounded claim(s): 第2句：工期90天')
  assert.equal(m, '写作核验未通过，以下内容没有对应证据或承诺：第2句：工期90天')
})

test('commitment errors keep the specific reason', () => {
  assert.match(d3ErrorMessage('commitment_unconfirmed', 'd2: commitment is not confirmed: non-price commitment S7 requires a tender requirement source') ?? '', /关联一条招标要求/)
  assert.match(d3ErrorMessage('commitment_unconfirmed', 'd2: commitment is not confirmed: commitment S7 requires unit, owner and rationale') ?? '', /单位/)
  assert.match(d3ErrorMessage('commitment_unconfirmed', 'd3: x: token [[COMMIT:9]] (not allowed)') ?? '', /未列入故事板/)
})

test('unrelated codes fall through to the caller', () => {
  assert.equal(d3ErrorMessage('revision_conflict', 'x'), undefined)
  assert.equal(d3ErrorMessage(undefined, 'x'), undefined)
})
