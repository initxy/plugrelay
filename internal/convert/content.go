package convert

import "github.com/initxy/plugrelay/pkg/ir"

// movedImageNote replaces a tool result whose only content was images.
const movedImageNote = "(image output attached below)"

// stepContent moves images out of tool results for targets that cannot carry
// them there. The images follow the run of tool results they came from, in
// the same user message, because targets usually require tool results to be
// contiguous.
func stepContent(req *ir.Request, c *Context, r *recorder) {
	if c.Target.ToolResultImgs {
		return
	}
	moved := false
	for i := range req.Messages {
		m := &req.Messages[i]
		var out, pending []ir.Block
		flush := func() {
			out = append(out, pending...)
			pending = nil
		}
		for _, b := range m.Content {
			if b.Type != ir.BlockToolResult {
				flush()
				out = append(out, b)
				continue
			}
			if b.Result != nil {
				var keep, imgs []ir.Block
				for _, cb := range b.Result.Content {
					if cb.Type == ir.BlockImage {
						imgs = append(imgs, cb)
					} else {
						keep = append(keep, cb)
					}
				}
				if len(imgs) > 0 {
					if len(keep) == 0 {
						keep = []ir.Block{{Type: ir.BlockText, Text: movedImageNote}}
					}
					res := *b.Result
					res.Content = keep
					b.Result = &res
					pending = append(pending, imgs...)
					moved = true
				}
			}
			out = append(out, b)
		}
		flush()
		m.Content = out
	}
	if moved {
		r.step("content: tool result images moved after the results")
		r.drop("tool_result_images")
	}
}
