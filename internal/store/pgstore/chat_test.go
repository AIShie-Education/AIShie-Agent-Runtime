package pgstore

// Include additive chat tables in the shared storetest database reset without
// editing the legacy migration/test table list.
func init() {
	tables = append(tables, "chat_approval_action", "chat_approval", "chat_generation", "chat_idempotency", "chat_attachment", "chat_draft", "chat_message", "chat_conversation")
}
