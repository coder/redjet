package redjet

// pipeline contains state of a Redis pipeline.
type pipeline struct {
	at  int
	end int
}
