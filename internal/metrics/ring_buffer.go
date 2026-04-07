package metrics

// ringAppend adds an item to a ring buffer slice, evicting the oldest entry
// if the slice has reached its maximum capacity.
func ringAppend[T any](buf []T, max int, item T) []T {
	if len(buf) >= max {
		copy(buf, buf[1:])
		buf = buf[:len(buf)-1]
	}
	return append(buf, item)
}
