package core

// HostedSearchReceipt describes a single completed search and the answer spans
// grounded in its returned sources. It is neither a tool declaration nor a grant.
type HostedSearchReceipt struct {
	ID        string
	Query     string
	Sources   []WebSearchSource
	Citations []WebSearchCitation
	CreatedAt int64 // Unix seconds, for the Responses envelope
	Request   *HostedWebSearchRequest
}

type WebSearchSource struct {
	URL, Title string
	// Opaque replay material from the issuer is required for Anthropic output.
	EncryptedContent string
}

type WebSearchCitation struct {
	SourceIndex int
	// Half-open Unicode character offsets in the final answer, not UTF-8 bytes.
	Start, End                int
	CitedText, EncryptedIndex string
}
