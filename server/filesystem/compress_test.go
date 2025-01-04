package filesystem

import (
	"context"
	"os"
	"testing"

	. "github.com/franela/goblin"
)

// Given an archive named test.{ext}, with the following file structure:
//
//	test/
//	|──inside/
//	|────finside.txt
//	|──outside.txt
//
// this test will ensure that it's being decompressed as expected
func TestFilesystem_DecompressFile(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Decompress", func() {
		for _, ext := range []string{"zip", "rar", "tar", "tar.gz"} {
			g.It("can decompress a "+ext, func() {
				// copy the file to the new FS
				c, err := os.ReadFile("./testdata/test." + ext)
				g.Assert(err).IsNil()
				err = rfs.CreateServerFile("./test."+ext, c)
				g.Assert(err).IsNil()

				// decompress
				err = fs.DecompressFile(context.Background(), "/", "test."+ext)
				g.Assert(err).IsNil()

				// make sure everything is where it is supposed to be
				_, err = rfs.StatServerFile("test/outside.txt")
				g.Assert(err).IsNil()

				st, err := rfs.StatServerFile("test/inside")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = rfs.StatServerFile("test/inside/finside.txt")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()
			})
		}

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_SpaceAvailableForDecompression(t *testing.T) {
    g := Goblin(t)
    fs, rfs := NewFs()

    g.Describe("SpaceAvailableForDecompression", func() {
        for _, ext := range []string{"zip", "rar", "tar", "tar.gz"} {
            g.It("should succeed when enough space is available for decompression of a "+ext, func() {
                fs.SetDiskLimit(2048)

                // copy the file to the new FS
                c, err := os.ReadFile("./testdata/test." + ext)
                g.Assert(err).IsNil()
                err = rfs.CreateServerFile("./test."+ext, c)
                g.Assert(err).IsNil()

                err = fs.SpaceAvailableForDecompression(context.Background(), "./", "test."+ext)
                g.Assert(err).IsNil()
            })

            g.It("should fail when not enough space is available for decompression of a "+ext, func() {
                fs.SetDiskLimit(12)

                // copy the file to the new FS
                c, err := os.ReadFile("./testdata/test_13b." + ext)
                g.Assert(err).IsNil()
                err = rfs.CreateServerFile("./test_13b."+ext, c)
                g.Assert(err).IsNil()

                err = fs.SpaceAvailableForDecompression(context.Background(), "./", "test_13b."+ext)
                g.Assert(err.Error()).Equal("filesystem: not enough disk space")
            })
        }
    })
}