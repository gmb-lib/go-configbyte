package configclient_test

import (
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-configbyte/configclient"
)

// A copy is answered only under the version it was stored with: a moved version
// is a miss, never the old bytes.
func TestACopyIsAnsweredOnlyUnderItsOwnVersion(t *testing.T) {
	c := configclient.NewCache(10)
	c.Put("t1", "orders", "sha256:a", []byte(`{"v":1}`))

	body, ok := c.Get("t1", "orders", "sha256:a")
	qt.Assert(t, qt.IsTrue(ok))
	qt.Assert(t, qt.Equals(string(body), `{"v":1}`))

	_, ok = c.Get("t1", "orders", "sha256:b")
	qt.Assert(t, qt.IsFalse(ok))
}

// Two organisations never share a copy, even when their owners answer the same
// version string — the organisation is part of the key, not something inferred
// from the version.
func TestOrganisationsNeverShareACopy(t *testing.T) {
	c := configclient.NewCache(10)
	c.Put("t1", "orders", "sha256:same", []byte(`{"tenant":"one"}`))

	_, ok := c.Get("t2", "orders", "sha256:same")
	qt.Assert(t, qt.IsFalse(ok))
}

// Sections of one organisation are kept apart.
func TestSectionsAreKeptApart(t *testing.T) {
	c := configclient.NewCache(10)
	c.Put("t1", "orders", "sha256:same", []byte(`{"orders":true}`))

	_, ok := c.Get("t1", "stock", "sha256:same")
	qt.Assert(t, qt.IsFalse(ok))
}

// A caller that cannot name the organisation or the version is never answered
// and never stores — so a session without an organisation cannot fill, or read,
// a shared slot.
func TestAnEmptyOrganisationOrVersionNeverMatchesOrStores(t *testing.T) {
	c := configclient.NewCache(10)
	c.Put("", "orders", "sha256:a", []byte(`{}`))
	c.Put("t1", "orders", "", []byte(`{}`))
	qt.Assert(t, qt.Equals(c.Len(), 0))

	c.Put("t1", "orders", "sha256:a", []byte(`{}`))
	_, ok := c.Get("", "orders", "sha256:a")
	qt.Assert(t, qt.IsFalse(ok))
	_, ok = c.Get("t1", "orders", "")
	qt.Assert(t, qt.IsFalse(ok))
}

// A newer copy replaces the one held, in the same slot.
func TestANewerCopyReplacesTheOldOne(t *testing.T) {
	c := configclient.NewCache(10)
	c.Put("t1", "orders", "sha256:a", []byte(`{"v":1}`))
	c.Put("t1", "orders", "sha256:b", []byte(`{"v":2}`))
	qt.Assert(t, qt.Equals(c.Len(), 1))

	_, ok := c.Get("t1", "orders", "sha256:a")
	qt.Assert(t, qt.IsFalse(ok))
	body, ok := c.Get("t1", "orders", "sha256:b")
	qt.Assert(t, qt.IsTrue(ok))
	qt.Assert(t, qt.Equals(string(body), `{"v":2}`))
}

// The bound holds, and what goes first is the copy used least recently.
func TestTheBoundEvictsTheLeastRecentlyUsedCopy(t *testing.T) {
	c := configclient.NewCache(2)
	c.Put("t1", "orders", "sha256:1", []byte(`1`))
	c.Put("t2", "orders", "sha256:2", []byte(`2`))

	// Reading t1 makes t2 the oldest.
	_, ok := c.Get("t1", "orders", "sha256:1")
	qt.Assert(t, qt.IsTrue(ok))

	c.Put("t3", "orders", "sha256:3", []byte(`3`))
	qt.Assert(t, qt.Equals(c.Len(), 2))

	_, ok = c.Get("t2", "orders", "sha256:2")
	qt.Assert(t, qt.IsFalse(ok), qt.Commentf("the least recently used copy must be the one evicted"))
	_, ok = c.Get("t1", "orders", "sha256:1")
	qt.Assert(t, qt.IsTrue(ok))
	_, ok = c.Get("t3", "orders", "sha256:3")
	qt.Assert(t, qt.IsTrue(ok))
}

// A bound of zero turns the cache off: nothing is kept, everything misses.
func TestABoundOfZeroKeepsNothing(t *testing.T) {
	c := configclient.NewCache(0)
	c.Put("t1", "orders", "sha256:a", []byte(`{}`))
	qt.Assert(t, qt.Equals(c.Len(), 0))

	_, ok := c.Get("t1", "orders", "sha256:a")
	qt.Assert(t, qt.IsFalse(ok))
}
