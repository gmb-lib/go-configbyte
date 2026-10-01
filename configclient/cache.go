package configclient

import (
	"container/list"
	"sync"
)

// Cache keeps, per organisation, the last copy of each owner's section it was
// answered, under the version the owner stamped on it.
//
// A copy is only ever handed out against a version the owner has just
// confirmed: the cache answers "do I already hold these bytes?", never "what is
// the configuration?". Nothing in it expires by time — a version that has not
// moved means content that has not moved — and a bound on the number of copies
// keeps a deployment with many organisations from holding all of them at once;
// the copy used least recently goes first.
//
// A section holds nothing that depends on who is asking, which is what makes one
// copy per organisation safe to share among its members. The zero value is not
// usable; build one with NewCache.
type Cache struct {
	mu      sync.Mutex
	max     int
	order   *list.List // front = most recently used
	entries map[key]*list.Element
}

type key struct {
	tenant, section string
}

type entry struct {
	key     key
	version string
	body    []byte
}

// NewCache builds a cache holding at most max copies. A max of zero or less
// keeps nothing: every lookup misses and every store is dropped, which turns the
// cache off without changing any caller.
func NewCache(maxCopies int) *Cache {
	return &Cache{max: maxCopies, order: list.New(), entries: map[key]*list.Element{}}
}

// Get answers the copy held for this organisation's section when it was stored
// under exactly this version. An empty organisation or version never matches,
// so a caller that could not tell whose configuration it is reading can never be
// handed somebody else's.
func (c *Cache) Get(tenant, section, version string) ([]byte, bool) {
	if tenant == "" || version == "" {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[key{tenant, section}]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if e.version != version {
		return nil, false
	}
	c.order.MoveToFront(el)

	return e.body, true
}

// Put stores the copy of an organisation's section under the version its owner
// answered with those very bytes, replacing whatever copy was held before.
func (c *Cache) Put(tenant, section, version string, body []byte) {
	if c.max <= 0 || tenant == "" || version == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	k := key{tenant, section}
	if el, ok := c.entries[k]; ok {
		e := el.Value.(*entry)
		e.version, e.body = version, body
		c.order.MoveToFront(el)

		return
	}

	c.entries[k] = c.order.PushFront(&entry{key: k, version: version, body: body})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*entry).key)
	}
}

// Len answers how many copies are held.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.order.Len()
}
