package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestValidFixtures(t *testing.T) {
	testEnv(t)
	files, _ := filepath.Glob(fixture("valid_*.yaml"))
	if len(files) != 6 {
		t.Fatalf("want 6 valid fixtures, got %d", len(files))
	}
	for _, f := range files {
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

func TestInvalidFixtures(t *testing.T) {
	testEnv(t)
	want := map[string][]string{
		"invalid_unknown_key.yaml":           {"not found in type"},
		"invalid_unknown_model_key.yaml":     {`unknown key "argz"`},
		"invalid_duplicate_model.yaml":       {`duplicate model id|already defined`},
		"invalid_listen_hostname.yaml":       {"must be an IP literal"},
		"invalid_listen_wildcard.yaml":       {"not a loopback address"},
		"invalid_listen_tailnet.yaml":        {"not a loopback address"},
		"invalid_port_equal.yaml":            {"must differ from the server.listen port"},
		"invalid_duration_bare.yaml":         {"has no unit"},
		"invalid_unknown_var.yaml":           {"unknown environment variable %NO_SUCH_VAR_LOCALLM%"},
		"invalid_unterminated_percent.yaml":  {"unterminated %"},
		"invalid_relative_path.yaml":         {"must be an absolute path"},
		"invalid_drive_relative_path.yaml":   {"must be an absolute path"},
		"invalid_root_relative_path.yaml":    {"must be an absolute path"},
		"invalid_missing_file.yaml":          {"does not exist"},
		"invalid_not_gguf.yaml":              {"must have extension .gguf"},
		"invalid_missing_server_exe.yaml":    {"llama.server_exe: required"},
		"invalid_reserved_flag.yaml":         {`flag "-m" is reserved for the host`},
		"invalid_reserved_port.yaml":         {`flag "--port" is reserved for the host`},
		"invalid_models_prefix_flag.yaml":    {"router-mode flag"},
		"invalid_shard_not_first.yaml":       {"is not the first shard"},
		"invalid_shard_missing.yaml":         {"lonely-00003-of-00003.gguf", "is missing"},
		"invalid_model_id.yaml":              {"invalid model id"},
		"invalid_default_model_unknown.yaml": {`swap.default_model: "ghost" is not a key of models`},
		"invalid_mmproj_relative.yaml":       {"--mmproj", "must be an absolute path"},
		"invalid_mmproj_missing_value.yaml":  {"flag --mmproj needs a file path"},
		"invalid_mmproj_eq_relative.yaml":    {"--mmproj", "must be an absolute path"},
		"invalid_ctx_size.yaml":              {"is not a non-negative integer"},
		"invalid_no_models.yaml":             {"at least one model is required"},
		"invalid_range_values.yaml": {"llama.port", "llama.load_timeout", "llama.stop_timeout", "llama.log_lines",
			"swap.swap_cooldown", "swap.wait_timeout", "swap.vram_free_timeout", "swap.vram_margin_mib", "swap.max_queue",
			"swap.failed_ttl", "vram.source", "logging.dir", "logging.level", "logging.max_size_mib", "logging.max_files"},
	}
	files, _ := filepath.Glob(fixture("invalid_*.yaml"))
	if len(files) != len(want) {
		t.Fatalf("fixtures on disk %d, table %d", len(files), len(want))
	}
	for _, f := range files {
		name := filepath.Base(f)
		_, err := Load(f)
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		for _, w := range want[name] {
			if !regexp.MustCompile(w).MatchString(err.Error()) && !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not mention %q", name, err, w)
			}
		}
	}
}

func TestMissingConfigFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !os.IsNotExist(errorsUnwrapAll(err)) {
		t.Fatalf("want not-exist error, got %v", err)
	}
}

func errorsUnwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func TestReservedFlagsInBothArgumentLists(t *testing.T) {
	testEnv(t)
	for _, flag := range []string{"-m", "--model", "--host", "--port", "--api-key", "--api-key-file", "-hf", "--hf-repo", "-mu", "--model-url", "--models-dir"} {
		for _, suffix := range []string{"", "=value"} {
			for _, list := range []string{"defaults", "model"} {
				t.Run(flag+suffix+"/"+list, func(t *testing.T) {
					args := "['" + flag + suffix + "']"
					text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
					if list == "defaults" {
						text += "  default_args: " + args + "\n"
					}
					text += "models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
					if list == "model" {
						text += "    args: " + args + "\n"
					}
					if _, err := loadText(t, text); err == nil || !strings.Contains(err.Error(), flag) {
						t.Fatalf("reserved flag accepted: %v", err)
					}
				})
			}
		}
	}
}

// F2-D1: llama.cpp заменяет '_' на '-' в аргументах с "--"; -hfr и --no-models-* — синонимы из --help сборки 12128.
func TestReservedFlagSpellingsAndAliases(t *testing.T) {
	testEnv(t)
	const router = "router-mode flag --models-* is not allowed"
	for _, tc := range []struct{ arg, want string }{
		{"--api_key", `flag "--api-key" is reserved`},
		{"--api_key_file", `flag "--api-key-file" is reserved`},
		{"--hf_repo", `flag "--hf-repo" is reserved`},
		{"--model_url", `flag "--model-url" is reserved`},
		{"-hfr", `flag "-hfr" is reserved`},
		{"--models_dir", router},
		{"--no-models-autoload", router},
		{"--no_models_autoload", router},
	} {
		for _, suffix := range []string{"", "=value"} {
			for _, list := range []string{"defaults", "model"} {
				t.Run(tc.arg+suffix+"/"+list, func(t *testing.T) {
					args := "['" + tc.arg + suffix + "']"
					text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
					if list == "defaults" {
						text += "  default_args: " + args + "\n"
					}
					text += "models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
					if list == "model" {
						text += "    args: " + args + "\n"
					}
					if _, err := loadText(t, text); err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("want %q, got %v", tc.want, err)
					}
				})
			}
		}
	}
}

// reject-equals-form: текст ошибки формы --flag=value (без поля и номера аргумента).
const equalsForm = "the --flag=value form is not allowed; put the value in the next list item"

// reject-equals-form и strict-equals-form (решения владельца 2026-11-09, PV5 спецификации v2): любой аргумент,
// который начинается с '-' и содержит '=', отвергается в обоих списках; значения с '=' без '-' впереди проходят.
func TestEqualsFormArguments(t *testing.T) {
	testEnv(t)
	for _, tc := range []struct{ name, args, want string }{ // want == "" — конфиг загружается
		{"long", "['--hf-token=hf_abc_def']", "[0]: " + equalsForm},
		{"long underscore name", "['--hf_token=hf_abc_def']", "[0]: " + equalsForm},
		{"mixed case", "['--Hf_Token=x']", "[0]: " + equalsForm},
		{"ctx-size", "['--ctx-size=4096']", "[0]: " + equalsForm},
		{"short", "['-c=4096']", "[0]: " + equalsForm},
		{"short secret flag", "['-hft=hf_abc_def']", "[0]: " + equalsForm},
		{"empty value", "['--jinja=']", "[0]: " + equalsForm},
		{"index", "['-c', '4096', '--alias=tiny']", "[2]: " + equalsForm},
		// strict-equals-form: формы, которые пропускало прежнее правило (после "-" не латинская буква или в имени другие символы).
		{"dot in name", "['--hf.token=hf_abc_def']", "[0]: " + equalsForm},
		{"non-latin name", "['--tökén=hf_abc_def']", "[0]: " + equalsForm},
		{"space in name", "['--hf token=hf_abc_def']", "[0]: " + equalsForm},
		{"underscore first", "['--_x=1']", "[0]: " + equalsForm},
		{"digit first", "['--1x=1']", "[0]: " + equalsForm},
		{"triple dash", "['---x=1']", "[0]: " + equalsForm},
		{"short digit", "['-1=x']", "[0]: " + equalsForm},
		{"dash only", "['-=x']", "[0]: " + equalsForm},
		{"double dash only", "['--=x']", "[0]: " + equalsForm},
		{"dash value after flag", "['--alias', '-1=a']", "[1]: " + equalsForm},
		{"override-kv value", "['--override-kv', 'tokenizer.ggml.add_bos_token=bool:false']", ""},
		{"override-tensor value", "['-ot', 'exps=CPU']", ""},
		{"alias value", "['--alias', 'a=b']", ""},
		{"json value", `['--chat-template-kwargs', '{"enable_thinking":false}']`, ""},
		{"lone dash", "['-']", ""},
		{"lone double dash", "['--']", ""},
		{"negative number", "['--seed', '-1']", ""},
	} {
		for _, list := range []string{"defaults", "model"} {
			t.Run(tc.name+"/"+list, func(t *testing.T) {
				text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
				field := "models.tiny.args"
				if list == "defaults" {
					text += "  default_args: " + tc.args + "\n"
					field = "llama.default_args"
				}
				text += "models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
				if list == "model" {
					text += "    args: " + tc.args + "\n"
				}
				_, err := loadText(t, text)
				if tc.want == "" {
					if err != nil {
						t.Fatalf("want no error, got %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), field+tc.want) {
					t.Fatalf("want %q, got %v", field+tc.want, err)
				}
			})
		}
	}
}

func TestFileArgumentFlags(t *testing.T) {
	testEnv(t)
	for _, flag := range []string{"--mmproj", "-mm", "--model-draft", "-md", "--lora", "--chat-template-file",
		"--spec-draft-model", "--model_draft", "--spec_draft_model", "--chat_template_file",
		"--control-vector", "--control_vector", "--grammar-file", "--grammar_file", "-jf", "--json-schema-file",
		"--json_schema_file", "-lcs", "--lookup-cache-static", "--lookup_cache_static"} {
		for _, tc := range []struct {
			name, value string
			valid       bool
		}{
			{"existing", `%LOCALLM_TEST_DIR%\mmproj.gguf`, true},
			{"relative", `mmproj.gguf`, false},
			{"missing file", `%LOCALLM_TEST_DIR%\absent.gguf`, false},
			{"directory", `%LOCALLM_TEST_DIR%`, false},
		} {
			for _, equals := range []bool{false, true} {
				t.Run(flag+"/"+tc.name+map[bool]string{false: "/separate", true: "/equals"}[equals], func(t *testing.T) {
					args := "['" + flag + "', '" + tc.value + "']"
					if equals {
						args = "['" + flag + "=" + tc.value + "']"
					}
					text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    args: " + args + "\n"
					_, err := loadText(t, text)
					// reject-equals-form: форма --flag=value отвергается при любом значении; раздельная — по правилам пути.
					valid := tc.valid && !equals
					if (err == nil) != valid {
						t.Fatalf("valid=%t err=%v", valid, err)
					}
					if equals && !strings.Contains(err.Error(), "models.tiny.args[0]: "+equalsForm) {
						t.Fatalf("want %q in %v", equalsForm, err)
					}
				})
			}
		}
	}
}

// Секреты из переменных окружения не должны попадать в тексты ошибок (они уходят в журнал и MessageBox).
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	testEnv(t)
	const secret = "SENTINEL-secret-Zq7Xv9"
	const literalSecret = "LITERAL-sentinel-Kp3Wm"
	t.Setenv("LOCALLM_TEST_TOKEN", secret)
	cases := []struct{ name, args, want string }{
		{"reserved-underscore-eq", "['--api_key=%LOCALLM_TEST_TOKEN%']", `flag "--api-key" is reserved`},
		{"router-no-models-eq", "['--no_models_autoload=%LOCALLM_TEST_TOKEN%']", "router-mode flag --models-* is not allowed"},
		{"model-draft-underscore", "['--model_draft', '%LOCALLM_TEST_TOKEN%']", "--model-draft: value must be an absolute path"},
		{"ctx-size-underscore-bad", "['--ctx_size', '%LOCALLM_TEST_TOKEN%']", "flag --ctx-size: value is not a non-negative integer"},
		{"reserved-eq", "['--api-key=%LOCALLM_TEST_TOKEN%']", `flag "--api-key" is reserved`},
		{"reserved-port", "['--port=%LOCALLM_TEST_TOKEN%']", `flag "--port" is reserved`},
		{"router-eq", "['--models-dir=%LOCALLM_TEST_TOKEN%']", "router-mode flag"},
		{"router-prefix", "['--models-x=%LOCALLM_TEST_TOKEN%']", "router-mode flag --models-* is not allowed"},
		{"ctx-bad", "['-c', '%LOCALLM_TEST_TOKEN%']", "flag -c: value is not a non-negative integer"},
		{"unterminated", "['--foo=" + literalSecret + "%x']", "unterminated %"},
		{"mmproj-eq", "['--mmproj=%LOCALLM_TEST_TOKEN%']", "--mmproj: value must be an absolute path"},
		{"mmproj-split", "['--mmproj', '%LOCALLM_TEST_TOKEN%']", "--mmproj: value must be an absolute path"},
		{"mmproj-abs-missing", "['--mmproj=C:\\nonexistent-dir\\%LOCALLM_TEST_TOKEN%.gguf']", "--mmproj: file does not exist"},
		{"lora-then-token", "['--lora', '--hf-token=%LOCALLM_TEST_TOKEN%']", "--lora: value must be an absolute path"},
		{"router-models-var", "['--models-%LOCALLM_TEST_TOKEN%']", "router-mode flag --models-* is not allowed"},
		{"ctx-size-bad", "['--ctx-size', '%LOCALLM_TEST_TOKEN%']", "flag --ctx-size: value is not a non-negative integer"},
		{"equals-long", "['--hf-token=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-underscore", "['--hf_token=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-short", "['-hft=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-var-in-name", "['--%LOCALLM_TEST_TOKEN%=x']", equalsForm},
		{"equals-literal", "['--hf-token=" + literalSecret + "']", equalsForm},
		{"equals-dot-name", "['--hf.token=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-underscore-first", "['--_hf_token=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-dash-value", "['--alias', '-1=%LOCALLM_TEST_TOKEN%']", equalsForm},
		{"equals-non-latin-literal", "['--tökén=" + literalSecret + "']", equalsForm},
		{"control-vector-second-part", "['--control-vector', '%LOCALLM_TEST_DIR%\\mmproj.gguf,%LOCALLM_TEST_TOKEN%']", "--control-vector: value must be an absolute path"},
		{"lora-second-part-missing", "['--lora', '%LOCALLM_TEST_DIR%\\mmproj.gguf,C:\\nonexistent-dir\\%LOCALLM_TEST_TOKEN%.gguf']", "--lora: file does not exist"},
		{"grammar-file-underscore", "['--grammar_file', '%LOCALLM_TEST_TOKEN%']", "--grammar-file: value must be an absolute path"},
		{"json-schema-short", "['-jf', '%LOCALLM_TEST_TOKEN%']", "-jf: value must be an absolute path"},
		{"lookup-cache-static-underscore", "['--lookup_cache_static', '%LOCALLM_TEST_TOKEN%']", "--lookup-cache-static: value must be an absolute path"},
	}
	for _, c := range cases {
		for _, list := range []string{"defaults", "model"} {
			t.Run(c.name+"/"+list, func(t *testing.T) {
				text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
				if list == "defaults" {
					text += "  default_args: " + c.args + "\n"
				}
				text += "models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
				if list == "model" {
					text += "    args: " + c.args + "\n"
				}
				_, err := loadText(t, text)
				if err == nil {
					t.Fatal("want error")
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), literalSecret) {
					t.Fatalf("secret leaked: %v", err)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Fatalf("want %q in %v", c.want, err)
				}
			})
		}
	}
}

// Значение файлового аргумента, указывающее на каталог, не должно попадать в ошибку.
func TestFileArgDirectoryErrorDoesNotLeakPath(t *testing.T) {
	testEnv(t)
	dir := filepath.Join(t.TempDir(), "SENTINEL-dir-Hq4Ty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALLM_TEST_SECRETDIR", dir)
	for _, args := range []string{"['--mmproj=%LOCALLM_TEST_SECRETDIR%']", "['--mmproj', '%LOCALLM_TEST_SECRETDIR%']"} {
		text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    args: " + args + "\n"
		_, err := loadText(t, text)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "value is a directory, not a file") {
			t.Fatalf("want directory error in %v", err)
		}
		if strings.Contains(err.Error(), filepath.Base(dir)) {
			t.Fatalf("path leaked: %v", err)
		}
	}
}

func TestFieldError(t *testing.T) {
	if got := (FieldError{"models.a.args[1]", "--mmproj: file does not exist"}).Error(); got != "models.a.args[1]: --mmproj: file does not exist" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (FieldError{Message: "no field"}).Error(); got != "no field" {
		t.Fatalf("Error() = %q", got)
	}
	var fe FieldError
	if !errors.As(fmt.Errorf("wrap: %w", FieldError{"f", "m"}), &fe) || fe.Field != "f" || fe.Message != "m" {
		t.Fatalf("errors.As: %+v", fe)
	}
	testEnv(t)
	_, err := loadText(t, serverText("", "")+"    args: ['--grammar_file', 'relative']\n")
	if !errors.As(err, &fe) || fe.Field != "models.tiny.args[1]" || fe.Message != "--grammar-file: value must be an absolute path" {
		t.Fatalf("Load must preserve the field and argument index: %v", err)
	}
}

// serverText — конфиг с моделью tiny: server — строки внутри server: после listen, top — ключи верхнего уровня перед models.
func serverText(server, top string) string {
	return "server:\n  listen: 127.0.0.1:8080\n" + server +
		"llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n" + top +
		"models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
}

// §5.2 спецификации v2: server.admin_key_file и server.state_file — абсолютные пути; файл ключа администратора —
// не тот же файл, что ключи клиентов.
func TestServerAdminAndStateFiles(t *testing.T) {
	dir := testEnv(t)
	c, err := loadText(t, serverText("  admin_key_file: '%LOCALLM_TEST_DIR%\\admin.txt'\n  state_file: '%LOCALLM_TEST_DIR%\\st.json'\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.AdminKeyFile != dir+`\admin.txt` || c.Server.StateFile != dir+`\st.json` {
		t.Fatalf("admin_key_file %q, state_file %q", c.Server.AdminKeyFile, c.Server.StateFile)
	}
	for _, tc := range []struct{ name, server, want string }{
		{"relative admin key file", "  admin_key_file: 'admin.txt'\n", `server.admin_key_file: "admin.txt" must be an absolute path`},
		{"relative state file", "  state_file: 'state.json'\n", `server.state_file: "state.json" must be an absolute path`},
		{"same file as client keys", "  api_keys_file: '%LOCALLM_TEST_DIR%\\keys.txt'\n  admin_key_file: '%LOCALLM_TEST_DIR%\\KEYS.TXT'\n",
			"server.admin_key_file: must differ from server.api_keys_file"},
		{"same file through ..", "  api_keys_file: '%LOCALLM_TEST_DIR%\\keys.txt'\n  admin_key_file: '%LOCALLM_TEST_DIR%\\sub\\..\\keys.txt'\n",
			"server.admin_key_file: must differ from server.api_keys_file"},
		{"unknown variable", "  admin_key_file: '%NO_SUCH_VAR_LOCALLM%\\a.txt'\n",
			"server.admin_key_file: unknown environment variable %NO_SUCH_VAR_LOCALLM%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadText(t, serverText(tc.server, "")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// Разные имена существующего файла тоже не разделяют ключи администратора и клиентов.
	client, admin := filepath.Join(dir, "keys.txt"), filepath.Join(dir, "admin-link.txt")
	if err := os.WriteFile(client, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(client, admin); err != nil {
		t.Fatal(err)
	}
	_, err = loadText(t, serverText(fmt.Sprintf("  api_keys_file: '%s'\n  admin_key_file: '%s'\n", client, admin), ""))
	if err == nil || !strings.Contains(err.Error(), "server.admin_key_file: must differ from server.api_keys_file") {
		t.Fatalf("hard-linked keys: %v", err)
	}
}

// §5.2 спецификации v2: server.admin_hosts — имена DNS или IP-литералы без порта, не больше 16; хранятся в нижнем
// регистре, IPv6 без скобок.
func TestAdminHosts(t *testing.T) {
	testEnv(t)
	c, err := loadText(t, serverText("  admin_hosts: ['PC.tail1234.ts.net', 'localhost', '100.64.0.1', '[FD7A:115C::1]', '::1']\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pc.tail1234.ts.net", "localhost", "100.64.0.1", "fd7a:115c::1", "::1"}; !reflect.DeepEqual(c.Server.AdminHosts, want) {
		t.Fatalf("admin_hosts %q", c.Server.AdminHosts)
	}
	const bad = " is not a DNS name or an IP literal (no port)"
	for i, tc := range []struct{ hosts, want string }{
		{"['pc:8080']", `server.admin_hosts[0]: "pc:8080"` + bad},
		{"['ok', '[::1]:8080']", `server.admin_hosts[1]: "[::1]:8080"` + bad},
		{"['under_score']", `server.admin_hosts[0]: "under_score"` + bad},
		{"['']", `server.admin_hosts[0]: ""` + bad},
		{"['[1.2.3.4]']", `server.admin_hosts[0]: "[1.2.3.4]"` + bad},
		{"['a..b']", `server.admin_hosts[0]: "a..b"` + bad},
		{"['http://pc']", `server.admin_hosts[0]: "http://pc"` + bad},
		{"['" + strings.Repeat("a.", 127) + "a']", bad},
		{"[" + strings.Repeat("'h', ", 16) + "'h']", "server.admin_hosts: at most 16 entries"},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := loadText(t, serverText("  admin_hosts: "+tc.hosts+"\n", "")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// §5.2 спецификации v2: models_dirs хранятся в каноническом виде; нет каталога — предупреждение, каталог пропущен.
func TestModelsDirs(t *testing.T) {
	dir := testEnv(t)
	if err := os.Mkdir(filepath.Join(dir, "Models"), 0o755); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(dir, "link"), filepath.Join(dir, "Models"))
	canon, err := Canonical(filepath.Join(dir, "Models"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadText(t, serverText("", "models_dirs: ['%LOCALLM_TEST_DIR%\\models', '%LOCALLM_TEST_DIR%\\no-such-models-dir', '%LOCALLM_TEST_DIR%\\link']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ModelsDirs, []string{canon, canon}) {
		t.Fatalf("models_dirs %q, want canonical %q twice", c.ModelsDirs, canon)
	}
	wantWarn := fmt.Sprintf("models_dirs[1]: directory %q does not exist or is not available; skipped", dir+`\no-such-models-dir`)
	if !reflect.DeepEqual(c.Warnings, []string{wantWarn}) {
		t.Fatalf("warnings %q, want %q", c.Warnings, wantWarn)
	}
	for _, tc := range []struct {
		dirs string
		want []string
	}{
		{"['models']", []string{`models_dirs[0]: "models" must be an absolute path`}},
		{"['%LOCALLM_TEST_DIR%\\model.gguf']", []string{"models_dirs[0]: ", "is not a directory"}},
		{"[" + strings.Repeat("'%LOCALLM_TEST_DIR%', ", 16) + "'%LOCALLM_TEST_DIR%']", []string{"models_dirs: at most 16 entries"}},
		{"['%NO_SUCH_VAR_LOCALLM%']", []string{"models_dirs[0]: unknown environment variable %NO_SUCH_VAR_LOCALLM%"}},
	} {
		_, err := loadText(t, serverText("", "models_dirs: "+tc.dirs+"\n"))
		for _, w := range tc.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: want %q, got %v", tc.dirs, w, err)
			}
		}
	}
}

// §5.2 спецификации v2: models_file по умолчанию — models.yaml рядом с locallm.yaml; явное значение раскрывается
// и должно быть абсолютным.
func TestModelsFileKey(t *testing.T) {
	dir := testEnv(t)
	c, err := loadText(t, serverText("", "models_file: '%LOCALLM_TEST_DIR%\\my-models.yaml'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsFile != dir+`\my-models.yaml` {
		t.Fatalf("models_file %q", c.ModelsFile)
	}
	if _, err := loadText(t, serverText("", "models_file: 'models.yaml'\n")); err == nil ||
		!strings.Contains(err.Error(), `models_file: "models.yaml" must be an absolute path`) {
		t.Fatalf("relative models_file: %v", err)
	}
}

// Граница в 16 записей допустима для обоих новых списков.
func TestGlobalListLimitsAllowSixteen(t *testing.T) {
	dir := testEnv(t)
	c, err := loadText(t, serverText("  admin_hosts: ["+strings.Repeat("'localhost', ", 15)+"'localhost']\n",
		"models_dirs: ["+strings.Repeat("'%LOCALLM_TEST_DIR%', ", 15)+"'%LOCALLM_TEST_DIR%']\n"))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Server.AdminHosts) != 16 || len(c.ModelsDirs) != 16 || len(c.Warnings) != 0 || c.ModelsDirs[15] != canon {
		t.Fatalf("hosts %q, dirs %q, warnings %q", c.Server.AdminHosts, c.ModelsDirs, c.Warnings)
	}
}

// §5.5 п. 11 спецификации v2: значение --lora и --control-vector делится по запятым, каждая часть — отдельный путь;
// у остальных файловых флагов запятая — часть пути.
func TestListFileArgumentsSplitOnCommas(t *testing.T) {
	testEnv(t)
	for _, flag := range []string{"--lora", "--control-vector", "--control_vector"} {
		for _, tc := range []struct{ name, value, want string }{
			{"two files", `%LOCALLM_TEST_DIR%\mmproj.gguf,%LOCALLM_TEST_DIR%\model.gguf`, ""},
			{"relative second part", `%LOCALLM_TEST_DIR%\mmproj.gguf,model.gguf`, "value must be an absolute path"},
			{"missing second part", `%LOCALLM_TEST_DIR%\mmproj.gguf,%LOCALLM_TEST_DIR%\absent.gguf`, "file does not exist"},
			{"empty part", `%LOCALLM_TEST_DIR%\mmproj.gguf,`, "value must be an absolute path"},
			{"directory part", `%LOCALLM_TEST_DIR%\mmproj.gguf,%LOCALLM_TEST_DIR%`, "value is a directory, not a file"},
		} {
			t.Run(flag+"/"+tc.name, func(t *testing.T) {
				text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n" +
					"    args: ['-c', '4096', '" + flag + "', '" + tc.value + "']\n"
				_, err := loadText(t, text)
				if tc.want == "" {
					if err != nil {
						t.Fatalf("want no error, got %v", err)
					}
					return
				}
				want := "models.tiny.args[3]: " + normalizeFlag(flag) + ": " + tc.want
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("want %q, got %v", want, err)
				}
			})
		}
	}
	text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n" +
		"    args: ['--mmproj', '%LOCALLM_TEST_DIR%\\mmproj.gguf,%LOCALLM_TEST_DIR%\\model.gguf']\n"
	if _, err := loadText(t, text); err == nil || !strings.Contains(err.Error(), "models.tiny.args[1]: --mmproj: file does not exist") {
		t.Fatalf("--mmproj must not be split: %v", err)
	}
}

func TestFileArgRefs(t *testing.T) {
	refs, missing := fileArgRefs([]string{"-c", "4096", "--lora", `C:\a.gguf,C:\b.gguf`, `--mmproj=C:\m.gguf`,
		"--chat_template_file", `C:\t.jinja`, "--alias", "x", "-jf"})
	want := []fileArgRef{
		{Index: 3, Flag: "--lora", Value: `C:\a.gguf`},
		{Index: 3, Flag: "--lora", Value: `C:\b.gguf`},
		{Index: 4, Flag: "--mmproj", Value: `C:\m.gguf`},
		{Index: 6, Flag: "--chat-template-file", Value: `C:\t.jinja`},
	}
	if !reflect.DeepEqual(refs, want) || !reflect.DeepEqual(missing, []int{9}) {
		t.Fatalf("refs %+v missing %v", refs, missing)
	}
	// Как в MVP: значение после файлового флага тоже проверяется как флаг.
	refs, missing = fileArgRefs([]string{"--lora", "--mmproj"})
	if !reflect.DeepEqual(refs, []fileArgRef{{Index: 1, Flag: "--lora", Value: "--mmproj"}}) || !reflect.DeepEqual(missing, []int{1}) {
		t.Fatalf("refs %+v missing %v", refs, missing)
	}
}

// §5.5 п. 14 спецификации v2: ошибки типа и тега при разборе locallm.yaml не содержат значения. yaml.v3 v3.0.1
// вставляет скаляр в текст ошибки типа (до 10 символов целиком, длиннее — первые 7) и в текст ошибки тега (целиком).
func TestYAMLErrorsDoNotLeakValues(t *testing.T) {
	testEnv(t)
	const secret = "TOKEN123"
	head := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
	model := "models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
	for _, tc := range []struct{ name, text, want string }{
		{"default_args scalar", head + "  default_args: '" + secret + "'\n" + model, "line 3: cannot unmarshal !!str into []string"},
		{"default_args element with a tag", head + "  default_args: ['--alias', !!int " + secret + "]\n" + model,
			"yaml: cannot decode a !!str value as a !!int"},
		{"models_dirs scalar", head + "models_dirs: '" + secret + "'\n" + model, "line 3: cannot unmarshal !!str into []string"},
		{"models_dirs element with a tag", head + "models_dirs: [!!int " + secret + "]\n" + model,
			"yaml: cannot decode a !!str value as a !!int"},
		{"admin_hosts scalar", "server:\n  admin_hosts: '" + secret + "'\n" + head + model, "line 2: cannot unmarshal !!str into []string"},
		{"admin_hosts element with a tag", "server:\n  admin_hosts: [!!int " + secret + "]\n" + head + model,
			"yaml: cannot decode a !!str value as a !!int"},
		{"models_file with a tag", head + "models_file: !!int " + secret + "\n" + model, "yaml: cannot decode a !!str value as a !!int"},
		{"port, long value", head + "  port: " + secret + "99\n" + model, "line 3: cannot unmarshal !!str into int"},
		{"duration", head + "  load_timeout: " + secret + "\n" + model, "line 3: invalid duration (use e.g. 30s or 5m)"},
		{"model args scalar", head + model + "    args: '" + secret + "'\n", "models.tiny.args (line 6): must be a list of strings"},
		{"model args element with a tag", head + model + "    args: ['--alias', !!int " + secret + "]\n",
			"models.tiny.args (line 6): must be a list of strings"},
		{"model path with a tag", head + "models:\n  tiny:\n    path: !!int " + secret + "\n", "models.tiny.path (line 5): must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadText(t, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), secret[:7]) {
				t.Fatalf("value leaked: %v", err)
			}
		})
	}
	// Ключ не из схемы, отступ и синтаксис по-прежнему называются (в этих текстах значений нет).
	for _, tc := range []struct{ text, want string }{
		{head + "  typo: true\n" + model, "line 3: field typo not found in type config.rawLlama"},
		{head + "  load_timeout: 300\n" + model, "line 3: duration has no unit (write e.g. 30s)"},
		{head + "models: [\n", "yaml: yaml: line "},
	} {
		if _, err := loadText(t, tc.text); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
}

func TestSafeYAMLError(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"line 3: cannot unmarshal !!str `TOKEN123` into []string", "line 3: cannot unmarshal !!str into []string"},
		{"line 3: cannot unmarshal !!str `TOKEN12...` into int", "line 3: cannot unmarshal !!str into int"},
		{"line 3: cannot unmarshal !!str `a into b` into []string", "line 3: cannot unmarshal !!str into []string"},
		{"line 3: cannot unmarshal !!str `a\nb` into []string", "line 3: cannot unmarshal !!str into []string"},
		{"line 3: cannot unmarshal !!seq into string", "line 3: cannot unmarshal !!seq into string"},
		{"yaml: cannot decode !!str `TOKEN123` as a !!int", "yaml: cannot decode a !!str value as a !!int"},
		{"line 5: field typo not found in type config.rawLlama", "line 5: field typo not found in type config.rawLlama"},
		{"yaml: line 2: did not find expected key", "yaml: line 2: did not find expected key"},
		{"yaml: line 2: did not find expected ',' or ']'", "yaml: line 2: did not find expected ',' or ']'"},
		{"yaml: unknown anchor 'TOKEN123' referenced", "invalid value"},
		{"yaml: invalid map key: []interface {}{\"TOKEN123\"}", "invalid value"},
		{"line 4: something 'TOKEN123'", "line 4: invalid value"},
	} {
		if got := safeYAMLError(tc.in); got != tc.want {
			t.Errorf("safeYAMLError(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
