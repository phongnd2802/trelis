package types

type ParseRequest struct {
	FileContent []byte
	FileName    string
	FileType    string
	Title       string
}

type ParseResult struct {
	MarkdownContent string
	ImageRefs       []ImageRef
	ImageDirPath    string
	Metadata        map[string]string
	Error           string
	SourceBlocks    []SourceBlock
}

type ImageRef struct {
	Filename    string
	OriginalRef string
	MimeType    string
	StorageKey  string
	ImageData   []byte
	IsOriginal  bool
}
