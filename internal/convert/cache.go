package convert

import "github.com/initxy/plugrelay/pkg/ir"

// defaultTTL is the breakpoint TTL used when nothing says otherwise.
const defaultTTL = "5m"

// stepCache adapts prompt-cache hints to the target's cache mode.
//
// Placement policy for targets that need explicit breakpoints when the
// client's protocol has none: the last system block and the last cacheable
// block of each of the last two messages. Thinking, opaque and raw blocks
// cannot carry a breakpoint, so the nearest earlier block is used.
func stepCache(req *ir.Request, c *Context, r *recorder) {
	src, dst := cacheMode(c.Source.Cache), cacheMode(c.Target.Cache)
	has := hasMarks(req)
	switch dst {
	case ir.CacheExplicit:
		if has {
			if !c.Target.Cache1h && shortenTTL(req) {
				r.step("cache: 1h breakpoints shortened to " + defaultTTL)
				r.drop("cache_ttl_1h")
			}
			return
		}
		if src == ir.CacheExplicit {
			return // the client chose not to cache
		}
		if n := placeMarks(req); n > 0 {
			r.step("cache: placed breakpoints by policy")
		}
	case ir.CacheKeyed:
		if has {
			stripMarks(req)
			r.step("cache: breakpoints removed for keyed cache")
		}
		if src != ir.CacheKeyed && req.CacheKey == "" && c.SessionID != "" {
			req.CacheKey = c.SessionID
			r.step("cache: cache key set from session")
		}
	default:
		if has {
			stripMarks(req)
			r.step("cache: breakpoints removed")
			if dst == ir.CacheNone {
				r.drop("cache_breakpoints")
			}
		}
	}
}

// eachMark visits every cache mark slot in the request.
func eachMark(req *ir.Request, fn func(**ir.CacheMark)) {
	for i := range req.System {
		fn(&req.System[i].Cache)
	}
	for i := range req.Tools {
		fn(&req.Tools[i].Cache)
	}
	for i := range req.Messages {
		for j := range req.Messages[i].Content {
			b := &req.Messages[i].Content[j]
			fn(&b.Cache)
			if b.Result != nil {
				for k := range b.Result.Content {
					fn(&b.Result.Content[k].Cache)
				}
			}
		}
	}
}

func hasMarks(req *ir.Request) bool {
	found := false
	eachMark(req, func(m **ir.CacheMark) { found = found || *m != nil })
	return found
}

func stripMarks(req *ir.Request) {
	eachMark(req, func(m **ir.CacheMark) { *m = nil })
}

func shortenTTL(req *ir.Request) bool {
	changed := false
	eachMark(req, func(m **ir.CacheMark) {
		if *m != nil && (*m).TTL == "1h" {
			*m = &ir.CacheMark{TTL: defaultTTL}
			changed = true
		}
	})
	return changed
}

func placeMarks(req *ir.Request) int {
	n := 0
	if i := lastCacheable(req.System); i >= 0 {
		req.System[i].Cache = &ir.CacheMark{TTL: defaultTTL}
		n++
	}
	for m := len(req.Messages) - 1; m >= 0 && m >= len(req.Messages)-2; m-- {
		content := req.Messages[m].Content
		if i := lastCacheable(content); i >= 0 {
			content[i].Cache = &ir.CacheMark{TTL: defaultTTL}
			n++
		}
	}
	return n
}

func lastCacheable(blocks []ir.Block) int {
	for i := len(blocks) - 1; i >= 0; i-- {
		switch blocks[i].Type {
		case ir.BlockThinking, ir.BlockOpaque, ir.BlockRaw:
			continue
		}
		return i
	}
	return -1
}
