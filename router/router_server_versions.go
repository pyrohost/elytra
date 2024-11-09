package router

import (
	"net/http"
	"path"
	"os"
	"io"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/router/middleware"
)

func FileSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// get installed version
func getInstalledVersion(c *gin.Context) {
	// Get the requested server
	s := middleware.ExtractServer(c)

	fs := s.Filesystem()

	// get first environment variable that includes SERVER and .jar in the name/value
	variables := s.GetEnvironmentVariables()
	var jar string
	for _, variable := range variables {
		if strings.Contains(variable, "JAR") && strings.Contains(variable, ".jar") {
			parts := strings.SplitN(variable, "=", 2)
			if len(parts) == 2 {
				jar = parts[1]
			}

			break
		}
	}

	// check if libraries/net/minecraftforge/forge folder exists
	if _, err := fs.Stat("libraries/net/minecraftforge/forge"); err == nil {
		// get first folder in libraries/net/minecraftforge/forge
		files, err := fs.ListDirectory("libraries/net/minecraftforge/forge")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load files."})
			return
		}

		if len(files) > 0 {
			folder := files[0].Name()

			// get hash of -server.jar or -universal.jar inside the first folder
			files, err := fs.ListDirectory(path.Join("libraries/net/minecraftforge/forge", folder))

			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load files."})
				return
			}

			for _, file := range files {
				if strings.HasSuffix(file.Name(), "-server.jar") || strings.HasSuffix(file.Name(), "-universal.jar") {
					hash, err := FileSHA256(path.Join(fs.Path(), "libraries/net/minecraftforge/forge", folder, file.Name()))

					if err != nil {
						c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to get hash."})
						return
					}

					c.JSON(http.StatusOK, gin.H{"success": true, "hash": hash})
					return
				}
			}
		}
	}

	// check if libraries/net/neoforged/neoforge folder exists
	if _, err := fs.Stat("libraries/net/neoforged/neoforge"); err == nil {
		// get first folder in libraries/net/neoforged/neoforge
		files, err := fs.ListDirectory("libraries/net/neoforged/neoforge")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load files."})
			return
		}

		if len(files) > 0 {
			folder := files[0].Name()

			// get hash of -server.jar or -universal.jar inside the first folder
			files, err := fs.ListDirectory(path.Join("libraries/net/neoforged/neoforge", folder))

			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load files."})
				return
			}

			for _, file := range files {
				if strings.HasSuffix(file.Name(), "-server.jar") || strings.HasSuffix(file.Name(), "-universal.jar") {
					hash, err := FileSHA256(path.Join(fs.Path(), "libraries/net/neoforged/neoforge", folder, file.Name()))

					if err != nil {
						c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to get hash."})
						return
					}

					c.JSON(http.StatusOK, gin.H{"success": true, "hash": hash})
					return
				}
			}
		}
	}

	// hash server.jar, if it exists
	if _, err := fs.Stat(jar); err == nil {
		hash, err := FileSHA256(path.Join(fs.Path(), jar))

		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to get hash."})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "hash": hash})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": false, "error": "No version found."})
}