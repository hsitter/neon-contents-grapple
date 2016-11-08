/*
	Copyright 2016 Harald Sitter <sitter@kde.org>

	This program is free software; you can redistribute it and/or
	modify it under the terms of the GNU General Public License as
	published by the Free Software Foundation; either version 3 of
	the License or any later version accepted by the membership of
	KDE e.V. (or its successor approved by the membership of KDE
	e.V.), which shall act as a proxy defined in Section 14 of
	version 3 of the license.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/danwakefield/fnmatch"
	"github.com/gin-gonic/gin"

	_ "net/http/pprof"
)

var memprofile = flag.String("memprofile", "", "write memory profile to this file")

var db = NewDatabase()
var boltDb = db.bolt
var pools = make(map[string][]string)

func updateContents() {
	fmt.Println("updating neon")
	start := time.Now()
	neon := NewContents("http://archive.neon.kde.org/user/dists/xenial/main/Contents-amd64.gz")
	neon.Get()
	fmt.Println("neon took ", time.Since(start))
	fmt.Println("updating ubuntu")
	ubuntu := NewContents("http://archive.ubuntu.com/ubuntu/dists/xenial/Contents-amd64.gz")
	start = time.Now()
	ubuntu.Get()
	fmt.Println("ubuntu took ", time.Since(start))
	pools["neon"] = []string{neon.id, ubuntu.id}
}


type FindAction struct {
	bucket *bolt.Bucket
	path   []byte
}

type FindResult struct {
	path     string
	packages []string
}

func find_it(pattern string, input chan *FindAction, output chan *FindResult, wg *sync.WaitGroup) {
	defer wg.Done()
	for action := range input {
		path := string(action.path)
		matched := fnmatch.Match(pattern, path, 0)
		if matched {
			// TODO could just wrap this in a struct
			subBucket := action.bucket.Bucket(action.path)
			var packages []string
			subBucket.ForEach(func(pkg, v []byte) error {
				packages = append(packages, string(pkg))
				return nil
			})
			output <- &FindResult{path: string(path), packages: packages}
		}
	}
}

func Find(archive string, pattern string) map[string][]string {
	m := make(map[string][]string)

	err := boltDb.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(archive))

		input := make(chan *FindAction)
		output := make(chan *FindResult, 2048) // Cache up to 4 times as many results.

		go func() {
			b.ForEach(func(path, v []byte) error {
				input <- &FindAction{bucket: b, path: path}
				return nil
			})
			close(input)
		}()

		var findWg sync.WaitGroup
		for i := 0; i < 512; i++ {
			findWg.Add(1)
			go find_it(pattern, input, output, &findWg)
		}

		go func() {
			// Close output once everything is read. This will make the read loop exit
			// once everything is read.
			findWg.Wait()
			close(output)
		}()

		// Read results until
		for result := range output {
			m[result.path] = result.packages
		}

		return nil
	})
	if err != nil {
		panic(err)
	}

	return m
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func isPool(a string) bool {
	for k := range pools {
		if a == k {
			return true
		}
	}
	return false
}

func v1_archives(c *gin.Context) {
	c.JSON(http.StatusOK, db.GetKeys("archives"))
}

func v1_find(c *gin.Context) {
	query := c.Query("q")
	archive := strings.TrimPrefix(c.Param("archive"), "/")
	// Security... only allow querying actual archives. Not arbitrary buckets.
	if !stringInSlice(archive, db.GetKeys("archives")) {
		c.JSON(http.StatusOK, make(map[string]string))
		return
	}
	c.JSON(http.StatusOK, Find(archive, query))
}

func main() {
	flag.Parse()

	err := boltDb.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("archives"))
		return err
	})
	if err != nil {
		panic(err)
	}

	updateContents()
	updateTicker := time.NewTicker(3 * time.Hour)
	go func() {
		for {
			<-updateTicker.C
			updateContents()
		}
	}()

	if *memprofile != "" {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.WriteHeapProfile(f)
		f.Close()
		return
	}

	fmt.Println("Ready to rumble...")

	router := gin.Default()
	router.GET("/v1/archives", v1_archives)
	router.GET("/v1/find/*archive", v1_find)

	port := os.Getenv("PORT")
	if len(port) <= 0 {
		port = "8080"
	}

	router.Run("localhost:" + port)
}
