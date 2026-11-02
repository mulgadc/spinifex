package message_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamespaceValidate(t *testing.T) {
	for _, id := range []string{"q", "Q-0_z", strings.Repeat("a", message.MaxQueueIDLen)} {
		assert.NoErrorf(t, message.Namespace{Queue: message.QueueID(id)}.Validate(), "queue ID %q", id)
	}
	for _, id := range []string{"", "a.b", "a*", "a>", "a b", "é", strings.Repeat("a", message.MaxQueueIDLen+1)} {
		assert.ErrorIsf(t, message.Namespace{Queue: message.QueueID(id)}.Validate(), message.ErrInvalidArgument, "queue ID %q", id)
	}
}

// TestS6_ContractImportsNoStoreTypes holds ADR-0011:S6, under which no
// JetStream type, subject, stream name or option crosses the interface.
func TestS6_ContractImportsNoStoreTypes(t *testing.T) {
	dirs := []string{".", "conformance"}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		require.NotEmpty(t, files, "no Go files under %s", dir)
		for _, file := range files {
			src, err := os.ReadFile(file)
			require.NoError(t, err)
			f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
			require.NoError(t, err)
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				require.NoError(t, err)
				assert.NotContainsf(t, path, "nats", "ADR-0011:S6: %s imports %s; the store contract and its suite stay store-neutral, so put store code in an implementation package", file, path)
			}
		}
	}
}
