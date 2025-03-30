package q

type Q interface {
	Connect() error
	Consume() error
	Publish(payload []byte)
	Close()
}
