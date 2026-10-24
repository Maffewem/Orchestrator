package agent

import "testing"

func TestDotenv(t *testing.T) {
	got, err := dotenv([]string{"A=plain", "B=has $dollar # and spaces", `C=it's "quoted" $x`})
	if err != nil {
		t.Fatal(err)
	}
	want := "A='plain'\nB='has $dollar # and spaces'\nC=\"it's \\\"quoted\\\" \\$x\"\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := dotenv([]string{"BAD KEY=1"}); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := dotenv([]string{"A=one\ntwo"}); err == nil {
		t.Fatal("newline accepted")
	}
}
