// context_blocks_peers.go — peer-awareness proprioception block for the foveated context.
//
// buildPeersBlock calls the same render path as GET /v1/peer-awareness and
// injects the pre-rendered packet inline with the foveated context frame.
// This closes the awareness loop for non-hook consumers — harness dispatches
// (cog_dispatch_to_harness), bus-native agents, and mod3 voice sessions —
// that never fire a Claude Code UserPromptSubmit hook and therefore never see
// the peer packet unless it is part of the foveated frame itself.
//
// Design notes:
//   - Reuses RenderPeerAwarenessPacket verbatim. No new composition logic.
//   - When the rendered packet is empty (no peers, no handoffs, no coord
//     chatter within the window) the block collapses to a single line so it
//     costs near zero tokens on a quiet workspace.
//   - Default token budget is DefaultPeerBlockBudget (~300 tokens), intentionally
//     more conservative than the HTTP endpoint's 500-token default. The block
//     shares the frame's overall budget, so being cheap here leaves room for
//     knowledge and events blocks.
//   - Emits the same peer_awareness.rendered beacon that the HTTP/MCP endpoints
//     emit, so the anti-echo machinery covers foveation injection too (per
//     issue body).
//   - Never panics. All errors degrade to a nil block (caller skips the block
//     entirely) or the collapsed "no peers" single line.
//   - include_peers defaults to true; a foveated request can suppress peer
//     injection by setting IncludePeers=false in the peer request opts.
package engine

import (
	"strings"
)

// DefaultPeerBlockBudget is the token budget allocated to BlockPeers inside
// the foveated frame. Kept below the HTTP endpoint's DefaultPeerAwarenessBudget
// (500) because the block shares the frame's budget envelope alongside
// BlockHealth, BlockField, and BlockKnowledge. Conservative but tunable.
const DefaultPeerBlockBudget = 300

// buildPeersBlock renders the peer-awareness packet for the given sid and
// returns it as a foveated context block. Returns nil when:
//   - sid is empty or fails ValidateSid (no session context available)
//   - RenderPeerAwarenessPacket returns an error
//
// Returns a block with a single "No active peers." line when the rendered
// packet is empty — this lets consumers distinguish "block ran but found
// nothing" from "block was skipped entirely".
//
// The peerAwarenessDeps bundle is assembled by the caller (serve_foveated.go)
// from the live Server dependencies, exactly as the HTTP handler does.
func buildPeersBlock(sid string, deps peerAwarenessDeps, budgetTokens int) *ContextBlock {
	if sid == "" {
		return nil
	}
	if err := ValidateSid(sid); err != nil {
		return nil
	}

	if budgetTokens <= 0 {
		budgetTokens = DefaultPeerBlockBudget
	}
	if budgetTokens > MaxPeerAwarenessBudget {
		budgetTokens = MaxPeerAwarenessBudget
	}

	req := PeerAwarenessRequest{
		Sid:          sid,
		Budget:       budgetTokens,
		IncludePeers: true,
	}
	// normalizePeerAwarenessRequest fills in Window and Now defaults.

	result, err := RenderPeerAwarenessPacket(deps, req)
	if err != nil {
		return nil
	}

	content := renderPeersBlockContent(result)
	block := NewBlock(BlockPeers, content)
	return &block
}

// renderPeersBlockContent wraps the peer-awareness packet in a markdown
// section header. When the packet is empty the section collapses to a single
// informational line so the block exists but costs almost nothing.
func renderPeersBlockContent(result *PeerAwarenessResult) string {
	var sb strings.Builder
	sb.WriteString("## Peer Awareness\n\n")

	if result == nil || strings.TrimSpace(result.Packet) == "" {
		sb.WriteString("No active peers.\n")
		return sb.String()
	}

	sb.WriteString(result.Packet)
	sb.WriteString("\n")
	return sb.String()
}
