package paperless

// Document contains the Paperless fields used by reconciliation and workers.
// Checksum is Paperless's checksum field and identifies the source document.
type Document struct {
	ID       int    `json:"id"`
	Content  string `json:"content"`
	Checksum string `json:"checksum"`
	Tags     []int  `json:"tags"`
}

// Tag is a Paperless document tag.
type Tag struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type documentPage struct {
	Count    int        `json:"count"`
	Next     *string    `json:"next"`
	Previous *string    `json:"previous"`
	Results  []Document `json:"results"`
}

type tagPage struct {
	Count    int     `json:"count"`
	Next     *string `json:"next"`
	Previous *string `json:"previous"`
	Results  []Tag   `json:"results"`
}
