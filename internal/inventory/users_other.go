//go:build !darwin

package inventory

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/host"
)

// signedIn is who is signed in to this computer now, as its system says.
func signedIn(ctx context.Context) []User {
	users, err := host.UsersWithContext(ctx)
	if err != nil {
		return nil
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		user := User{Name: u.User, Terminal: u.Terminal}
		if u.Started > 0 {
			user.Since = time.Unix(int64(u.Started), 0).UTC()
		}
		out = append(out, user)
	}
	return out
}
