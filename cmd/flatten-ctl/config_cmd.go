package main

import (
	"flag"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/remote"
	"gopkg.in/yaml.v3"
)

// cmdConfig implements `flatten-ctl config` — emit a normalized flatten config
// (FLATTEN_CONFIG) from --config (loading also validates it), or a commented
// skeleton via --template. Mirrors `sandbox-ctl config`.
//
//	--config <file>   load + normalize + re-emit (or FLATTEN_CONFIG env)
//	--template        emit a commented skeleton instead
//	-o <file>         write to file (default stdout)
func cmdConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	configPath := fs.String("config", "", "input flatten config YAML (overrides FLATTEN_CONFIG env)")
	template := fs.Bool("template", false, "emit a commented skeleton config instead of reading --config")
	out := fs.String("o", "", "write output to this file instead of stdout")
	fs.Parse(args)

	var output []byte
	if *template {
		output = []byte(flattenSkeleton)
	} else {
		cfg, err := remote.LoadConfig(*configPath, flattenConfigEnv) // LoadConfig normalizes (= validates)
		if err != nil {
			fatal("%v", err)
		}
		b, err := yaml.Marshal(cfg)
		if err != nil {
			fatal("%v", err)
		}
		output = b
	}

	if *out != "" {
		if err := os.WriteFile(*out, output, 0o644); err != nil {
			fatal("%v", err)
		}
		return
	}
	os.Stdout.Write(output)
}

// flattenSkeleton is the commented authoring template for FLATTEN_CONFIG.
const flattenSkeleton = `# flatten config — flatten-ctl export --config <this> (or FLATTEN_CONFIG env).
# All fields optional; shown with defaults. Applies to registry sources.
tmpdir: ""                 # parent of per-run scratch dirs ("" -> $TMPDIR or /tmp)
platform: ""               # os/arch[/variant] to pull ("" -> host linux/<arch>); --platform overrides
insecure: false            # allow plain-HTTP registries (dev/private); does NOT affect TLS cert verification
pull_jobs: 4               # concurrent layer downloads
tls:                       # HTTPS cert verification (registry + CDN blob redirects)
  ca_cert: ""              # path to extra CA bundle (PEM) to trust, e.g. an intercepting proxy's root CA
  insecure_skip_verify: false # disable cert verification entirely (insecure; prefer ca_cert)
cache:
  dir: ""                  # persistent blob cache dir ("" -> ephemeral, removed after the run)
  max_size: "10GiB"        # cache cap ("" -> default, "0" -> unlimited)
referer:
  enabled: false           # default-enable the idempotent OCI-Referrers flow (= --with-referer)
  desc: ""                 # public owner descriptor (annotation)
  key: ""                  # HMAC message paired with the customer key (defaults to desc)
  validity: ""             # optional Go duration -> referrer expiry
`
