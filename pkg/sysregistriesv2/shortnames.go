package sysregistriesv2

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/docker/docker/pkg/homedir"
	"github.com/pkg/errors"
)

// defaultShortNameMode is the default mode of registries.conf files if the
// corresponding field is left empty.
const defaultShortNameMode = types.ShortNameModePermissive

// userShortNamesFile is the user-specific config file to store aliases.
var userShortNamesFile = filepath.FromSlash("containers/short-name-aliases.conf")

// shortNameAliasesConfPath returns the path to the machine-generated
// short-name-aliases.conf file.
func shortNameAliasesConfPath(ctx *types.SystemContext) (string, error) {
	if ctx != nil && len(ctx.UserShortNameAliasConfPath) > 0 {
		return ctx.UserShortNameAliasConfPath, nil
	}

	configHome, err := homedir.GetConfigHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(configHome, userShortNamesFile), nil
}

// alias combines the parsed value of an alias with the config file it has been
// specified in.  The config file is crucial for an improved user experience
// such that users are able to resolve potential pull errors.
type alias struct {
	// The parsed value of an alias.  May be nil if set to "" in a config.
	value reference.Named
	// The config file the alias originates from.
	configOrigin string
}

// shortNameAliasConf is a subset of the `V2RegistriesConf` format.  It's used in the
// software-maintained `userShortNamesFile`.
type shortNameAliasConf struct {
	// A map for aliasing short names to their fully-qualified image
	// reference counter parts.
	// Note that Aliases is niled after being loaded from a file.
	Aliases map[string]string `toml:"aliases"`

	// Note that an alias value may be nil iff it's set as an empty string
	// in the config.
	namedAliases map[string]alias
}

// ResolveShortNameAlias performs an alias resolution of the specified name.
// The user-specific short-name-aliases.conf has precedence over aliases in the
// assembled registries.conf.  It returns the possibly resolved alias or nil, a
// human-readable description of the config where the alias is specified, and
// an error. The origin of the config file is crucial for an improved user
// experience such that users are able to resolve potential pull errors.
// Almost all callers should use pkg/shortnames instead.
//
// Note that it’s the caller’s responsibility to pass only a repository
// (reference.IsNameOnly) as the short name.
func ResolveShortNameAlias(ctx *types.SystemContext, name string) (reference.Named, string, error) {
	if err := validateShortName(name); err != nil {
		return nil, "", err
	}
	confPath, lock, err := shortNameAliasesConfPathAndLock(ctx)
	if err != nil {
		return nil, "", err
	}

	// Acquire the lock as a reader to allow for multiple routines in the
	// same process space to read simultaneously.
	lock.RLock()
	defer lock.Unlock()

	aliasConf, err := loadShortNameAliasConf(confPath)
	if err != nil {
		return nil, "", err
	}

	// First look up the short-name-aliases.conf.  Note that a value may be
	// nil iff it's set as an empty string in the config.
	alias, resolved := aliasConf.namedAliases[name]
	if resolved {
		return alias.value, alias.configOrigin, nil
	}

	config, err := getConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	alias, resolved = config.namedAliases[name]
	if resolved {
		return alias.value, alias.configOrigin, nil
	}
	return nil, "", nil
}

// editShortNameAlias loads the aliases.conf file and changes it. If value is
// set, it adds the name-value pair as a new alias. Otherwise, it will remove
// name from the config.
func editShortNameAlias(ctx *types.SystemContext, name string, value *string) error {
	if err := validateShortName(name); err != nil {
		return err
	}
	if value != nil {
		if _, err := parseShortNameValue(*value); err != nil {
			return err
		}
	}

	confPath, lock, err := shortNameAliasesConfPathAndLock(ctx)
	if err != nil {
		return err
	}

	// Acquire the lock as a writer to prevent data corruption.
	lock.Lock()
	defer lock.Unlock()

	// Load the short-name-alias.conf, add the specified name-value pair,
	// and write it back to the file.
	conf, err := loadShortNameAliasConf(confPath)
	if err != nil {
		return err
	}

	if value != nil {
		conf.Aliases[name] = *value
	} else {
		// If the name does not exist, throw an error.
		if _, exists := conf.Aliases[name]; !exists {
			return errors.Errorf("short-name alias %q not found in %q: please check registries.conf files", name, confPath)
		}

		delete(conf.Aliases, name)
	}

	f, err := os.OpenFile(confPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	encoder := toml.NewEncoder(f)
	return encoder.Encode(conf)
}

// AddShortNameAlias adds the specified name-value pair as a new alias to the
// user-specific aliases.conf.  It may override an existing alias for `name`.
//
// Note that it’s the caller’s responsibility to pass only a repository
// (reference.IsNameOnly) as the short name.
func AddShortNameAlias(ctx *types.SystemContext, name string, value string) error {
	return editShortNameAlias(ctx, name, &value)
}

// RemoveShortNameAlias clears the alias for the specified name.  It throws an
// error in case name does not exist in the machine-generated
// short-name-alias.conf.  In such case, the alias must be specified in one of
// the registries.conf files, which is the users' responsibility.
//
// Note that it’s the caller’s responsibility to pass only a repository
// (reference.IsNameOnly) as the short name.
func RemoveShortNameAlias(ctx *types.SystemContext, name string) error {
	return editShortNameAlias(ctx, name, nil)
}

// parseShortNameValue parses the specified alias into a reference.Named.  The alias is
// expected to not be tagged or carry a digest and *must* include a
// domain/registry.
//
// Note that the returned reference is always normalized.
func parseShortNameValue(alias string) (reference.Named, error) {
	ref, err := reference.Parse(alias)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing alias %q", alias)
	}

	if _, ok := ref.(reference.Digested); ok {
		return nil, errors.Errorf("invalid alias %q: must not contain digest", alias)
	}

	if _, ok := ref.(reference.Tagged); ok {
		return nil, errors.Errorf("invalid alias %q: must not contain tag", alias)
	}

	named, ok := ref.(reference.Named)
	if !ok {
		return nil, errors.Errorf("invalid alias %q: must contain registry and repository", alias)
	}

	registry := reference.Domain(named)
	if !(strings.ContainsAny(registry, ".:") || registry == "localhost") {
		return nil, errors.Errorf("invalid alias %q: must contain registry and repository", alias)
	}

	// A final parse to make sure that docker.io references are correctly
	// normalized (e.g., docker.io/alpine to docker.io/library/alpine.
	named, err = reference.ParseNormalizedNamed(alias)
	return named, err
}

// validateShortName parses the specified `name` of an alias (i.e., the left-hand
// side) and checks if it's a short name and does not include a tag or digest.
func validateShortName(name string) error {
	repo, err := reference.Parse(name)
	if err != nil {
		return errors.Wrapf(err, "cannot parse short name: %q", name)
	}

	if _, ok := repo.(reference.Digested); ok {
		return errors.Errorf("invalid short name %q: must not contain digest", name)
	}

	if _, ok := repo.(reference.Tagged); ok {
		return errors.Errorf("invalid short name %q: must not contain tag", name)
	}

	named, ok := repo.(reference.Named)
	if !ok {
		return errors.Errorf("invalid short name %q: no name", name)
	}

	registry := reference.Domain(named)
	if strings.ContainsAny(registry, ".:") || registry == "localhost" {
		return errors.Errorf("invalid short name %q: must not contain registry", name)
	}
	return nil
}

// parseAndValidate parses and validates all entries in conf.Aliases and stores
// the results in conf.namedAliases.
func (conf *shortNameAliasConf) parseAndValidate(path string) error {
	if conf.Aliases == nil {
		conf.Aliases = make(map[string]string)
	}
	if conf.namedAliases == nil {
		conf.namedAliases = make(map[string]alias)
	}
	errs := []error{}
	for name, value := range conf.Aliases {
		if err := validateShortName(name); err != nil {
			errs = append(errs, err)
		}

		// Empty right-hand side values in config files allow to reset
		// an alias in a previously loaded config. This way, drop-in
		// config files from registries.conf.d can reset potentially
		// malconfigured aliases.
		if value == "" {
			conf.namedAliases[name] = alias{nil, path}
			continue
		}

		named, err := parseShortNameValue(value)
		if err != nil {
			// We want to report *all* malformed entries to avoid a
			// whack-a-mole for the user.
			errs = append(errs, err)
		} else {
			conf.namedAliases[name] = alias{named, path}
		}
	}
	var err error // nil if no errors
	for _, e := range errs {
		if err == nil {
			err = e
		} else {
			err = errors.Wrapf(err, "%v\n", e)
		}
	}
	return err
}

func loadShortNameAliasConf(confPath string) (*shortNameAliasConf, error) {
	conf := shortNameAliasConf{}

	_, err := toml.DecodeFile(confPath, &conf)
	if err != nil && !os.IsNotExist(err) {
		// It's okay if the config doesn't exist.  Other errors are not.
		return nil, errors.Wrapf(err, "error loading short-name aliases config file %q", confPath)
	}

	// Better safe than sorry: validate the machine-generated config.  The
	// file could still be corrupted by another process or user.
	if err := conf.parseAndValidate(confPath); err != nil {
		return nil, errors.Wrapf(err, "error loading short-name aliases config file %q", confPath)
	}

	return &conf, nil
}

func shortNameAliasesConfPathAndLock(ctx *types.SystemContext) (string, lockfile.Locker, error) {
	shortNameAliasesConfPath, err := shortNameAliasesConfPath(ctx)
	if err != nil {
		return "", nil, err
	}
	// Make sure the path to file exists.
	if err := os.MkdirAll(filepath.Dir(shortNameAliasesConfPath), 0700); err != nil {
		return "", nil, err
	}

	lockPath := shortNameAliasesConfPath + ".lock"
	locker, err := lockfile.GetLockfile(lockPath)
	return shortNameAliasesConfPath, locker, err
}
