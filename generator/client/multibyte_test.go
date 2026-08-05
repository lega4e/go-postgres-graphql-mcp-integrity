package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

// generateOps compiles one operation document against clientSchema and returns
// the generated source.
func generateOps(t *testing.T, ops string) (string, error) {
	t.Helper()
	doc, err := sdl.Parse(clientSchema)
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ops.graphql"), []byte(ops), 0o644))

	sources, err := Load(dir)
	require.NoError(t, err)
	files, err := Generate(doc, sources, Options{Package: "testclient"})
	if err != nil {
		return "", err
	}
	require.Len(t, files, 1)
	return string(files[0].Content), nil
}

// An operation is sliced out of its document by the position gqlparser
// recorded, and gqlparser counts positions in *runes*. Indexing the string with
// one is right only while every character is a single byte, so any multi-byte
// character ahead of an operation shifted the slice — an em dash (three bytes)
// by two, which handed the compiler the tail of the comment and produced
// `parse operation: operation:1:1: Unexpected Name "…"`.
//
// The characters here are the ones a comment actually collects: an em dash, an
// accent, a non-Latin script and an emoji, at two, three and four bytes.
func TestMultiByteCharactersAheadOfAnOperation(t *testing.T) {
	for name, lead := range map[string]string{
		"em dash":     "# steps — the workflow fan-out\n",
		"accent":      "# opération de lecture\n",
		"non-latin":   "# запрос списка\n",
		"emoji":       "# 🚀 hot path\n",
		"all at once": "# — é ы 🚀\n",
	} {
		t.Run(name, func(t *testing.T) {
			src, err := generateOps(t, lead+`query ListPeople($name: String!) {
  persons(name: $name) { name email }
}`)
			require.NoError(t, err)
			assert.Contains(t, src, "func (c *Client) ListPeople(")
		})
	}
}

// The same slice bounds the *end* of an operation, so a second operation after
// a multi-byte character has to come out whole too — a short end truncates the
// first operation rather than corrupting its start.
func TestMultiByteCharactersBetweenTwoOperations(t *testing.T) {
	src, err := generateOps(t, `# первый — list
query ListPeople($name: String!) {
  persons(name: $name) { name email }
}

# второй — append 🚀
mutation AppendEvent($streamId: String!, $payload: JSON!) {
  appendEvent(streamId: $streamId, payload: $payload)
}`)
	require.NoError(t, err)
	assert.Contains(t, src, "func (c *Client) ListPeople(")
	assert.Contains(t, src, "func (c *Client) AppendEvent(")
}

// A multi-byte character *inside* an operation — in a string default or an
// argument value — must survive the slice intact rather than being cut through
// the middle of a rune.
func TestMultiByteCharacterInsideAnOperation(t *testing.T) {
	src, err := generateOps(t, `mutation AppendEvent($streamId: String!, $payload: JSON!) {
  appendEvent(streamId: $streamId, payload: $payload, queue: "очередь—🚀")
}`)
	require.NoError(t, err)
	assert.Contains(t, src, `очередь—🚀`,
		"the operation text is baked into the client, so the value has to arrive uncut")
	assert.NotContains(t, src, "�", "no rune was cut in half")
}
