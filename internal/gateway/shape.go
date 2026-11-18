package gateway

import "strings"

// fieldPatch — поправка формы элемента /models или /v1/models под эталон router mode (§9.4).
// Пути — через точку от корня элемента ("status.failed"); для ключа "_top" — от корня ответа.
type fieldPatch struct {
	add  map[string]any // поля эталона без аналога у нас: нулевое значение типа эталона
	drop []string       // наши поля, которых нет в эталоне
}

// Поправки по итогам G7: TestG7ModelsMatchRouterMode печатает готовые литералы, если форма расходится.
// Ключ — статус ("loaded", "loading", "unloaded", "failed") или "_top".
var modelsShape = map[string]fieldPatch{
	"loaded":   {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "created": 0, "meta": map[string]any{"ftype": "", "n_ctx": 0, "n_ctx_train": 0, "n_embd": 0, "n_params": 0, "n_vocab": 0, "size": 0, "vocab_type": 0}, "object": "", "owned_by": "", "source": "", "status.preset": "", "tags": []any{}}, drop: []string{"path"}},
	"loading":  {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "created": 0, "object": "", "owned_by": "", "source": "", "status.preset": "", "tags": []any{}}, drop: []string{"path"}},
	"unloaded": {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "created": 0, "object": "", "owned_by": "", "source": "", "status.preset": "", "tags": []any{}}, drop: []string{"path"}},
	"failed":   {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "created": 0, "object": "", "owned_by": "", "source": "", "status.preset": "", "tags": []any{}}, drop: []string{"path"}},
	"_top":     {add: map[string]any{"object": ""}},
}

var v1ModelsShape = map[string]fieldPatch{
	"loaded":   {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "meta": map[string]any{"ftype": "", "n_ctx": 0, "n_ctx_train": 0, "n_embd": 0, "n_params": 0, "n_vocab": 0, "size": 0, "vocab_type": 0}, "source": "", "status.preset": "", "tags": []any{}}, drop: []string{}},
	"loading":  {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "source": "", "status.preset": "", "tags": []any{}}, drop: []string{}},
	"unloaded": {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "source": "", "status.preset": "", "tags": []any{}}, drop: []string{}},
	"failed":   {add: map[string]any{"aliases": []any{}, "architecture": map[string]any{"input_modalities": []any{}, "output_modalities": []any{}}, "can_remove": false, "source": "", "status.preset": "", "tags": []any{}}, drop: []string{}},
}

// failedValue — status.value для логического failed (§9.4: ожидается "unloaded" + failed + exit_code, 02 §1.2).
var failedValue = "unloaded"

// applyShape удаляет пути drop, затем добавляет недостающие пути add (копии значений).
func applyShape(obj map[string]any, p fieldPatch) {
	for _, path := range p.drop {
		deletePath(obj, path)
	}
	for path, v := range p.add {
		setPathIfMissing(obj, path, deepCopy(v))
	}
}

func deletePath(obj map[string]any, path string) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		next, ok := obj[k].(map[string]any)
		if !ok {
			return
		}
		obj = next
	}
	delete(obj, keys[len(keys)-1])
}

func setPathIfMissing(obj map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		next, ok := obj[k].(map[string]any)
		if !ok {
			if _, exists := obj[k]; exists {
				return // не объект — не трогаем
			}
			next = map[string]any{}
			obj[k] = next
		}
		obj = next
	}
	last := keys[len(keys)-1]
	if _, exists := obj[last]; !exists {
		obj[last] = v
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = deepCopy(x)
		}
		return a
	}
	return v
}
