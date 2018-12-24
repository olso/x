// +build ignore

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"strings"
)

func main() {
	for i := 1; i <= 3; i++ {
		n := fmt.Sprintf("copy%v", i)
		fn := fmt.Sprintf("example.com_%v_v1.0.0.txt", n)
		c := strings.Replace(tmpl, "copyN", n, -1)
		if err := ioutil.WriteFile(fn, []byte(c), 0666); err != nil {
			log.Fatalf("failed to generate %v: %v", fn, err)
		}
	}
}

const tmpl = `
-- .mod --
module example.com/copyN

-- .info --
{"Version":"v1.0.0","Time":"2018-10-22T18:45:39Z"}

-- go.mod --
module example.com/copyN

-- main.go --
package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	fFirst = flag.String("outdir:first", "", "where to put first files")
)

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) != 2 {
		log.Fatalf("expected 2 args; got %v", len(args))
	}

	n, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		log.Fatalf("failed to parse sleep time from %v: %v", args[0], err)
	}
	time.Sleep(time.Duration(n * float64(time.Second)))

	in := args[1]
	out := filepath.Join(filepath.Dir(in), "gen_"+filepath.Base(in)+"_copyN.go")

	inf, err := os.Open(in)
	if err != nil {
		log.Fatalf("failed to open %v: %v", in, err)
	}
	outf, err := os.Create(out)
	if err != nil {
		log.Fatalf("failed to open (for writing) %v: %v", out, err)
	}

	if _, err := io.Copy(outf, inf); err != nil {
		log.Fatalf("failed to copy from %v to %v: %v", in, out, err)
	}

	if err := outf.Close(); err != nil {
		log.Fatalf("failed to close %v: %v", out, err)
	}

	// create a run file
	ls, err := ioutil.ReadDir(".")
	if err != nil {
		log.Fatalf("failed to read current dir: %v", err)
	}
	max := 0
	for _, fi := range ls {
		if fi.IsDir() {
			continue
		}
		fn := fi.Name()
		if !strings.HasPrefix(fn, "copyN_run") {
			continue
		}
		n := strings.TrimPrefix(fn, "copyN_run")
		i, err := strconv.Atoi(n)
		if err != nil {
			log.Fatalf("failed to parse number from file %v", fn)
		}
		if i > max {
			max = i
		}
	}
	max += 1
	rf := fmt.Sprintf("copyN_run%v", max)
	if err := ioutil.WriteFile(rf, []byte{}, 0666); err != nil {
		log.Fatalf("failed to create run file %v: %v", rf, err)
	}
}
`
