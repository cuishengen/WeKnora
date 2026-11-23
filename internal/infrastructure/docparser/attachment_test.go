package docparser

import (
	"context"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"strings"
	"testing"
)

func TestUploadedArchiveIndexesOnlyItsCatalogue(t *testing.T) {
	for _, ext := range utils.AttachmentExtensions() {
		if !IsSimpleFormat(ext) {
			t.Fatalf("%s has no local reader", ext)
		}
		result, err := (&SimpleFormatReader{}).Read(context.Background(), &types.ReadRequest{FileName: "SDK." + ext, FileType: ext, FileContent: []byte("secret internal archive payload")})
		if err != nil || !strings.Contains(result.MarkdownContent, "SDK."+ext) || strings.Contains(result.MarkdownContent, "secret internal") {
			t.Fatalf("archive contents were treated as parsed text: %s", ext)
		}
	}
}
