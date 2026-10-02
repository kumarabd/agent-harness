// Package discordui holds Discord message-component helpers shared by the
// discord (text) and discordvoice platforms — chiefly the user-input
// button block, which both platforms' DeliverInterim activities render
// (voice posts it to the voice channel's attached text chat).
package discordui

import "github.com/bwmarrin/discordgo"

// discordui.BuildUserInputComponents turns a user_input_requests row's options into
// Discord message components (buttons), one row of up to 5 buttons each,
// max 5 rows (Discord's own limits). custom_id encodes the routing —
// the discord package's interaction handler parses it back apart on
// click. Deliberately hard-caps at 25 options: beyond that, a select-menu
// component would be the right shape, but no consumer today generates
// more than a handful of options, so accepting the cap now rather than
// adding an unused fallback path.
//
// The custom_id format ("user_input:<request_id>:<option_id>") stays
// under Discord's 100-char custom_id ceiling for realistic values —
// request_id is a UUID (36 chars) and option_id is a short string
// ("approve", "deny") in the only consumer that exists today (permission
// gating). If a future consumer introduces longer option ids, the button
// build call will still succeed but the click handler will reject
// oversized ids with a clean error — no silent truncation.
// BuildUserInputComponents — see package doc.
func BuildUserInputComponents(requestID string, options []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}) []discordgo.MessageComponent {
	const (
		maxButtonsPerRow = 5
		maxRows          = 5
		maxOptions       = maxButtonsPerRow * maxRows
	)
	if len(options) > maxOptions {
		options = options[:maxOptions]
	}
	var rows []discordgo.MessageComponent
	for i := 0; i < len(options); i += maxButtonsPerRow {
		end := i + maxButtonsPerRow
		if end > len(options) {
			end = len(options)
		}
		var buttons []discordgo.MessageComponent
		for _, opt := range options[i:end] {
			buttons = append(buttons, discordgo.Button{
				Label: opt.Label,
				Style: discordgo.PrimaryButton,
				// Prefix scopes this custom_id to the user-input feature —
				// the discord package's handler filters on this exact
				// prefix so we never accidentally handle a click for a
				// future unrelated button type. Colons are safe because
				// request_id (UUID) and option_id ("approve"/"deny"/...)
				// don't contain them in any consumer today.
				CustomID: "user_input:" + requestID + ":" + opt.ID,
			})
		}
		rows = append(rows, discordgo.ActionsRow{Components: buttons})
	}
	return rows
}
