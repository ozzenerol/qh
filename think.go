package main

// qh sends every earlier answer's thinking back as reasoning_content, with
// preserve_thinking on, so the Qwen template renders each request as an
// extension of the one before and the server's prompt cache holds. Without
// preserve_thinking the template drops the thinking of every assistant turn
// before the last user message, and qh's own user messages (self-check, fix
// turn) would then change the rendered prompt in the middle of a run.
//
// Thinking of earlier runs is only weight: the model does not need the way
// it reasoned about a finished task. At the start of a run qh removes it
// when there is enough of it to pay for the one cache miss this causes.

const (
	thinkDropMin   = 8000 // tokens of old thinking before qh drops it
	thinkDropShare = 15   // and at least this percent of the context
)

// dropOldThinking clears reasoning_content of the assistant messages so far
// (all of them belong to earlier runs) when they hold at least thinkDropMin
// tokens and thinkDropShare percent of the messages. It returns the tokens
// dropped, or 0.
func dropOldThinking(msgs []Message) int {
	think, all := 0, 0
	for _, m := range msgs {
		all += len(m.Content) + len(m.ReasoningContent)
		if m.Role == "assistant" {
			think += len(m.ReasoningContent)
		}
	}
	think, all = think/4, all/4
	if think < thinkDropMin || think*100 < all*thinkDropShare {
		return 0
	}
	for i := range msgs {
		msgs[i].ReasoningContent = ""
	}
	return think
}
