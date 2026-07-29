package ssm

import "testing"

func TestDeterministicCommentIsStableAndSafe(t *testing.T) {
	a := DeterministicComment("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "apply", "i-123")
	if a != DeterministicComment("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "apply", "i-123") {
		t.Fatal("comment is not deterministic")
	}
	if len(a) != len("dirextalk-core-v1:")+64 {
		t.Fatalf("unexpected comment length: %d", len(a))
	}
	if a == DeterministicComment("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab", "apply", "i-123") {
		t.Fatal("comment does not fence plan digest")
	}
}
