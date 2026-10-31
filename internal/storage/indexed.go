package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/storageuse"
)

func (s *scanner) indexed() {
	if s.opts.UseRoot == "" {
		return
	}
	err := storageuse.VisitResources(s.ctx, s.opts.UseRoot, s.b.Limits.Records, func() error { return s.b.entry(s.ctx) }, s.b.Charge, func(resource storageuse.Resource) error {
		path := filepath.Clean(resource.Location)
		if !filepath.IsAbs(path) {
			return errors.New("untrusted registration path")
		}
		owner, repository, identity := "unknown", "", ""
		useRepository := resource.Repository
		switch resource.Class {
		case "agent-build", "validate-build", "agent-deps", "validate-deps", "legacy-go-build":
			rel, err := filepath.Rel(s.opts.CacheRoot, path)
			parts := strings.Split(rel, string(filepath.Separator))
			class := resource.Class
			if class == "legacy-go-build" {
				class = "go-build"
			}
			if err != nil || !filepath.IsLocal(rel) || len(parts) < 2 || len(parts) > 3 || parts[0] != class {
				return errors.New("registration outside trusted cache layout")
			}
			if resource.Repository != parts[1] {
				useRepository = ""
			}
			identity = "cache:" + path
			if privateLayout(s.opts.CacheRoot, rel) {
				repository = s.cacheOwner(parts[1])
			}
			if resource.Class != "legacy-go-build" {
				owner = class
			} else if repository != "" {
				owner = repository
			}
		case "native-scratch", "sandbox-scratch", "prefetch-scratch", "review-scratch", "validation-scratch":
			rel, err := filepath.Rel(s.opts.ScratchRoot, path)
			valid := err == nil && filepath.IsLocal(rel) && !strings.ContainsRune(rel, filepath.Separator) && scratchClass(rel) == resource.Class
			if !valid && resource.Class == "prefetch-scratch" {
				cacheRel, cacheErr := filepath.Rel(s.opts.CacheRoot, path)
				parts := strings.Split(cacheRel, string(filepath.Separator))
				valid = cacheErr == nil && filepath.IsLocal(cacheRel) && len(parts) == 3 && (parts[0] == "agent-deps" || parts[0] == "validate-deps") && parts[1] == resource.Repository && strings.HasPrefix(parts[2], ".prefetch-rust-") && len(parts[2]) > len(".prefetch-rust-")
			}
			if !valid {
				return errors.New("registration outside trusted scratch layout")
			}
			// Validate the exact managed leaf; never follow arbitrary registered paths.
			if leaf, err := openExisting(path); err == nil {
				leaf.Close()
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			identity = "scratch:" + path
			for common, display := range s.repos {
				h := sha256.Sum256([]byte(filepath.Clean(common)))
				if hex.EncodeToString(h[:16]) == resource.Repository {
					repository = display
					break
				}
			}
		default:
			// Unknown classes and arbitrary contributor locations are not followed.
			return errors.New("unsupported indexed storage class")
		}
		if resource.Identity != identity {
			return errors.New("unrecognized indexed identity")
		}
		s.add(Resource{useRepository: useRepository, Repository: repository, Owner: owner, Class: resource.Class, Path: path, Identity: identity, Eligibility: "unknown", Reason: "durable registration observed; missing repository or use coverage does not establish abandonment", Evidence: "trusted control registration"})
		return s.ctx.Err()
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail("indexed registrations", err)
	}
}
