package openssh

import "testing"

func TestQuoteConfigValue(t *testing.T) {
	t.Parallel()
	quoted, err := QuoteConfigValue(`/Users/test/Library/Application Support/barn/"keys"\known_hosts`)
	if err != nil || quoted != `"/Users/test/Library/Application Support/barn/\"keys\"\\known_hosts"` {
		t.Fatalf("quoted=%q err=%v", quoted, err)
	}
	for _, value := range []string{"", "bad\npath", "bad\x00path"} {
		if _, err := QuoteConfigValue(value); err == nil {
			t.Fatalf("unsafe value %q accepted", value)
		}
	}
}
