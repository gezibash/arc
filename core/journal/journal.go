// Package journal defines transactional persistence needed by durable delivery.
package journal

// Store runs a whole callback atomically. Update commits only when fn succeeds;
// otherwise it rolls back. Views are read-only. Returned bytes live for the callback.
type Store interface {
	View(func(Tx) error) error
	Update(func(Tx) error) error
	Close()
}
type Tx interface {
	Bucket([]byte) Bucket
	CreateBucketIfNotExists([]byte) (Bucket, error)
}
type Bucket interface {
	Get([]byte) []byte
	Put([]byte, []byte) error
	Delete([]byte) error
	ForEach(func([]byte, []byte) error) error
	Cursor() Cursor
}
type Cursor interface {
	First() ([]byte, []byte)
	Next() ([]byte, []byte)
	Delete() error
}
