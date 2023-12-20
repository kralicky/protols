package util

func Map[T any, R any](collection []T, iteratee func(T) R) []R {
	result := make([]R, len(collection))

	for i, item := range collection {
		result[i] = iteratee(item)
	}

	return result
}

type Collection[T any] interface {
	Len() int
	Get(i int) T
}

func Collect[T any, C Collection[T]](c C) []T {
	l := c.Len()
	out := make([]T, 0, l)
	for i := 0; i < l; i++ {
		out = append(out, c.Get(i))
	}
	return out
}
