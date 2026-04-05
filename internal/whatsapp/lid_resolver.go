package whatsapp

import (
	"context"
	"log/slog"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// ResolveLID attempts to convert a LID (@lid) to its canonical PN (@s.whatsapp.net).
// Falls back to the original JID if resolution fails.
func ResolveLID(client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.Server != "lid" {
		return jid
	}

	pn, err := client.Store.LIDs.GetPNForLID(context.Background(), jid)
	if err == nil && !pn.IsEmpty() {
		slog.Debug("LID resolved to PN", "lid", jid.String(), "pn", pn.String())
		return pn
	}

	slog.Warn("LID could not be resolved", "lid", jid.String())
	return jid
}

// ResolveJIDString resolves a JID string, normalizing LIDs when possible.
func ResolveJIDString(client *whatsmeow.Client, jidStr string) string {
	if !IsLID(jidStr) {
		return jidStr
	}

	parsed, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr
	}

	resolved := ResolveLID(client, parsed)
	return resolved.String()
}

// IsLID returns true if the JID string is a Linked Device ID.
func IsLID(jid string) bool {
	return strings.HasSuffix(jid, "@lid")
}

// NormalizeJID returns the canonical form of a JID.
func NormalizeJID(jid types.JID) string {
	if jid.Server == "lid" {
		return jid.String() // keep LID if unresolved
	}
	if jid.Server == "s.whatsapp.net" || jid.Server == "" {
		return jid.User + "@s.whatsapp.net"
	}
	return jid.String()
}

// ResolveSender resolves the message sender JID and enriches push name if possible.
func ResolveSender(client *whatsmeow.Client, sender types.JID, pushName string) (string, string) {
	if sender.Server != "lid" {
		return NormalizeJID(sender), pushName
	}

	resolved := ResolveLID(client, sender)

	// Try to enrich push name from contact store if resolved
	if resolved.Server != "lid" && pushName == "" {
		contact, err := client.Store.Contacts.GetContact(context.Background(), resolved)
		if err == nil && contact.PushName != "" {
			pushName = contact.PushName
		}
	}

	return NormalizeJID(resolved), pushName
}

// ResolveChat resolves a chat JID (individual or group).
// Groups (@g.us) and broadcasts (@broadcast) are returned as-is.
func ResolveChat(client *whatsmeow.Client, chat types.JID) string {
	if chat.Server == "g.us" || chat.Server == "broadcast" {
		return chat.String()
	}
	if chat.Server == "lid" {
		resolved := ResolveLID(client, chat)
		return NormalizeJID(resolved)
	}
	return NormalizeJID(chat)
}
