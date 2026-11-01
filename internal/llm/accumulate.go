package llm

import (
	"encoding/json"
	"strings"
)

// Accumulator 把流事件按到达顺序组装成 assistant 消息。
type Accumulator struct {
	blocks     []ContentBlock
	toolInputs map[int]*strings.Builder // blocks 下标 -> 参数片段
	openTool   int                      // 当前未结束的 tool_use 下标，-1 表示无
	usage      Usage
	stopReason string
}

func NewAccumulator() *Accumulator {
	return &Accumulator{toolInputs: map[int]*strings.Builder{}, openTool: -1}
}

func (a *Accumulator) Add(ev StreamEvent) {
	switch ev.Type {
	case SETextDelta:
		a.closeTool()
		a.appendText(BlockText, ev.Text)
	case SEThinkingDelta:
		// 签名标志一个 thinking 块结束；之后的 thinking 增量（含空文本的块起始）开启新块，避免合并两个已签名块。
		a.closeTool()
		if n := len(a.blocks); n > 0 && a.blocks[n-1].Type == BlockThinking && a.blocks[n-1].Signature == "" {
			a.blocks[n-1].Text += ev.Text
		} else {
			a.blocks = append(a.blocks, ContentBlock{Type: BlockThinking, Text: ev.Text})
		}
	case SESignatureDelta:
		if n := len(a.blocks); n > 0 && a.blocks[n-1].Type == BlockThinking {
			a.blocks[n-1].Signature += ev.Text
		}
	case SERedactedThinking:
		a.closeTool()
		a.blocks = append(a.blocks, ContentBlock{Type: BlockRedactedThinking, Text: ev.Text})
	case SEResponseItem:
		a.closeTool()
		a.blocks = append(a.blocks, ContentBlock{Type: BlockResponseItem, Input: json.RawMessage(ev.Text)})
	case SEToolUseStart:
		a.closeTool()
		b := ContentBlock{Type: BlockToolUse}
		if ev.ToolUse != nil {
			b.ID, b.Name = ev.ToolUse.ID, ev.ToolUse.Name
		}
		a.blocks = append(a.blocks, b)
		a.openTool = len(a.blocks) - 1
		a.toolInputs[a.openTool] = &strings.Builder{}
	case SEToolUseDelta:
		if a.openTool >= 0 {
			a.toolInputs[a.openTool].WriteString(ev.Text)
		}
	case SEToolUseEnd:
		if a.openTool >= 0 && ev.ToolUse != nil && len(ev.ToolUse.Input) > 0 {
			a.blocks[a.openTool].Input = ev.ToolUse.Input
		}
		a.closeTool()
	case SEMessageStop:
		a.closeTool()
		if ev.Usage != nil {
			a.usage.Add(*ev.Usage)
		}
		if ev.StopReason != "" {
			a.stopReason = ev.StopReason
		}
	}
}

func (a *Accumulator) appendText(t BlockType, s string) {
	if s == "" {
		return
	}
	if n := len(a.blocks); n > 0 && a.blocks[n-1].Type == t {
		a.blocks[n-1].Text += s
		return
	}
	a.blocks = append(a.blocks, ContentBlock{Type: t, Text: s})
}

func (a *Accumulator) closeTool() {
	if a.openTool < 0 {
		return
	}
	b := &a.blocks[a.openTool]
	if len(b.Input) == 0 {
		raw := strings.TrimSpace(a.toolInputs[a.openTool].String())
		if raw == "" {
			raw = "{}"
		}
		b.Input = json.RawMessage(raw)
	}
	delete(a.toolInputs, a.openTool)
	a.openTool = -1
}

// Result 结束组装；未显式结束的工具调用按已收到片段收尾。
func (a *Accumulator) Result() (Message, string, Usage) {
	a.closeTool()
	stop := a.stopReason
	if stop == "" {
		stop = StopEndTurn
		for _, b := range a.blocks {
			if b.Type == BlockToolUse {
				stop = StopToolUse
				break
			}
		}
	}
	return Message{Role: RoleAssistant, Content: a.blocks}, stop, a.usage
}
