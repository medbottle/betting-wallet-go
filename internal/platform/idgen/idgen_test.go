package idgen

import "testing"

func TestNewIDIsVersion7AndOrdered(t *testing.T) {
	var g UUIDv7
	prev := g.NewID()
	for i := 0; i < 1000; i++ {
		id := g.NewID()
		if id.Version() != 7 {
			t.Fatalf("version = %d", id.Version())
		}
		if id.String() <= prev.String() {
			t.Fatalf("ids not increasing: %s then %s", prev, id)
		}
		prev = id
	}
}
