//go:build unix

package fdgauge

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

const supported = true

// census reads /dev/fd — a view of the calling process's own descriptors on
// both darwin and linux — and classifies each entry via fstat. Readdirnames
// rather than ReadDir: ReadDir stats every entry itself and aborts the whole
// listing on the first descriptor that closed mid-census, where this loop
// just skips it.
func census() (string, error) {
	f, err := os.Open("/dev/fd")
	if err != nil {
		return "", err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return "", err
	}
	var reg, dir, sock, fifo, chr, other int
	for _, name := range names {
		n, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(n, &st) != nil {
			// The /dev/fd handle itself, already closed — or a descriptor
			// that closed mid-census. A gauge tolerates the race.
			continue
		}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFREG:
			reg++
		case syscall.S_IFDIR:
			dir++
		case syscall.S_IFSOCK:
			sock++
		case syscall.S_IFIFO:
			fifo++
		case syscall.S_IFCHR:
			chr++
		default:
			other++
		}
	}
	total := reg + dir + sock + fifo + chr + other
	limit := "?"
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
		limit = strconv.FormatUint(uint64(lim.Cur), 10)
	}
	return fmt.Sprintf("%d open (limit %s): reg=%d dir=%d sock=%d fifo=%d chr=%d other=%d",
		total, limit, reg, dir, sock, fifo, chr, other), nil
}
