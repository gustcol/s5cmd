package command

import "testing"

func TestConnPoolSize(t *testing.T) {
	cases := []struct {
		workers, concurrency, want int
	}{
		{256, 5, 1280},
		{256, 0, 256}, // commands without --concurrency
		{16, 16, 256},
		{0, 5, 10}, // numworkers is raised to the minimum of 2
		{100000, 5, maxConnPoolSize},
	}
	for _, tc := range cases {
		if got := connPoolSize(tc.workers, tc.concurrency); got != tc.want {
			t.Errorf("connPoolSize(%d, %d) = %d, want %d", tc.workers, tc.concurrency, got, tc.want)
		}
	}
}
