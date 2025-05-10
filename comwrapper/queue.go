package comwrapper

import (
	"sync"
)

// Queue �~X��~@个并�~O~Q�~I�~E��~Z~D�~X~_�~H~W�~S�~^~D
type Queue struct {
	items []interface{}
	lock  sync.Mutex
}

// NewQueue �~H~[建�~@个�~V��~Z~D�~X~_�~H~W
func NewQueue() *Queue {
	return &Queue{
		items: make([]interface{}, 0),
		lock:  sync.Mutex{},
	}
}

// Enqueue �~F�~E~C�| �~T��~E��~X~_�~H~W尾�~C�
func (q *Queue) Enqueue(item interface{}) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.items = append(q.items, item)
}

// Dequeue �~N�~X~_�~H~W头�~C��~O~V�~G��~E~C�|
func (q *Queue) Dequeue() interface{} {
	q.lock.Lock()
	defer q.lock.Unlock()

	if len(q.items) == 0 {
		return nil
	}

	item := q.items[0]
	q.items = q.items[1:]
	return item
}

// Len �~T�~[~^�~X~_�~H~W中�~E~C�| �~Z~D个�~U�
func (q *Queue) Len() int {
	q.lock.Lock()
	defer q.lock.Unlock()

	return len(q.items)
}
