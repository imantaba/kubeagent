# Go Concepts — kubeagent learning companion

A running cheat-sheet of the Go concepts used in this project. Each concept gets
a **plain everyday example first**, then the **kubeagent example**. This file
grows as the project does.

> Mental model: Go is compiled (you build a single binary), statically typed
> (types are checked before the program runs), and deliberately small — there are
> only a handful of concepts, and this project uses most of them.

---

## 1. Packages

A **package** is a folder of Go files that belong together. Every `.go` file
starts with `package <name>`. Code in one package uses code from another by
*importing* it.

A name is **exported** (usable from other packages) if it starts with a **capital
letter**. Lowercase names are private to their package. That capitalization *is*
Go's entire access-control system.

**Simple example:**

```go
package mathx

func Double(n int) int { return n * 2 } // exported (capital D)
func half(n int) int   { return n / 2 } // private (lowercase h)
```

**kubeagent example:** the project is split into packages by job —
`cluster`, `collect`, `diagnose`, `report`. `Detector` and `Finding` are
capitalized so other packages can use them; helper functions inside a package
stay lowercase.

> `internal/` is a special folder name: packages inside it can only be imported
> by code in the same module. We put our packages there because kubeagent is a
> tool, not a shared library.

---

## 2. Modules (`go.mod`)

A **module** is the unit you build and version. `go mod init <name>` creates a
`go.mod` file that records the module's import path and its dependencies. When
you import a third-party package, Go records the exact version in `go.mod` /
`go.sum`.

**Simple example:**

```
module github.com/you/hello
go 1.26
```

**kubeagent example:** `module github.com/imantaba/kubeagent`. When we add the
Kubernetes client, `go get k8s.io/client-go@...` records it here automatically.

---

## 3. Structs

A **struct** is a bundle of named fields — a custom type that groups related
data.

**Simple example:**

```go
type Point struct {
    X int
    Y int
}

p := Point{X: 3, Y: 4}
fmt.Println(p.X) // 3
```

**kubeagent example:** `Finding` groups the parts of one diagnosis:

```go
type Finding struct {
    Pod      string
    Issue    string
    Reason   string
    Evidence string
}
```

---

## 4. Methods and receivers

A **method** is a function attached to a type. The `(d CrashLoopDetector)` part
before the name is the **receiver** — it says "this function belongs to that
type."

**Simple example:**

```go
type Rectangle struct{ W, H int }

func (r Rectangle) Area() int { // Area is a method on Rectangle
    return r.W * r.H
}

box := Rectangle{W: 3, H: 4}
fmt.Println(box.Area()) // 12
```

**kubeagent example:** `Detect` is a method on each detector type:

```go
func (d CrashLoopDetector) Detect(facts PodFacts) *Finding { ... }
```

---

## 5. Interfaces (the big one)

An **interface** is a *contract*: a list of method signatures. **Any type that
has those methods automatically satisfies the interface** — there is no
`implements` keyword and no inheritance. If it has the method, it counts.

**Simple example — a set of validation rules:**

```go
type Problem struct{ Message string }

// A Rule checks a value and returns a *Problem, or nil if it's fine.
type Rule interface {
    Check(value string) *Problem
}

type NotEmpty struct{}
func (r NotEmpty) Check(value string) *Problem {
    if value == "" {
        return &Problem{Message: "must not be empty"}
    }
    return nil
}

type MinLength struct{ Min int }
func (r MinLength) Check(value string) *Problem {
    if len(value) < r.Min {
        return &Problem{Message: fmt.Sprintf("must be at least %d chars", r.Min)}
    }
    return nil
}
```

You can keep a list of rules and run them all, even though they are different
types — because they all satisfy `Rule`:

```go
rules := []Rule{ NotEmpty{}, MinLength{Min: 8} }
for _, rule := range rules {
    if p := rule.Check("hi"); p != nil {
        fmt.Println("problem:", p.Message)
    }
}
```

**kubeagent example — the exact same shape, renamed:**

- `Rule` → `Detector`
- `Problem` → `Finding`
- `Check(value string)` → `Detect(facts PodFacts)`
- `NotEmpty{}`, `MinLength{Min: 8}` → `CrashLoopDetector{}`, `OOMDetector{}`

```go
type Detector interface {
    Detect(facts PodFacts) *Finding
}

detectors := []Detector{
    CrashLoopDetector{}, ImagePullDetector{}, OOMDetector{}, PendingDetector{},
}
```

> Note: `NotEmpty{}` has no fields (it needs no config); `MinLength{Min: 8}` has
> a field because it carries config. Same with detectors — most need no config,
> but one could later carry, say, a memory threshold.

---

## 6. Pointers and `nil` as "optional"

A **pointer** (`*T`) is a reference to a value rather than a copy of it. A
pointer can be `nil`, meaning "points to nothing." A common Go pattern is to
return `*T` and use `nil` to mean **"no value / not found"**.

**Simple example:**

```go
// findEven returns a pointer to the first even number, or nil if none.
func findEven(nums []int) *int {
    for _, n := range nums {
        if n%2 == 0 {
            return &n        // &n = "a pointer to n"
        }
    }
    return nil               // nothing found
}

if p := findEven([]int{1, 3, 5}); p == nil {
    fmt.Println("no even number")
}
```

**kubeagent example:** `Detect` returns `*Finding`. A real finding means "this
pod has this problem"; `nil` means "this detector found nothing here."

```go
if finding := d.Detect(facts); finding != nil {
    findings = append(findings, *finding) // *finding reads the value the pointer points to
}
```

---

## 7. Slices, `append`, and `range`

A **slice** is Go's growable list (`[]T` is "a slice of T"). `append` adds to it;
`range` iterates over it.

**Simple example:**

```go
var names []string                 // an empty slice
names = append(names, "ann")       // ["ann"]
names = append(names, "bob")       // ["ann", "bob"]

for i, name := range names {
    fmt.Println(i, name)           // 0 ann / 1 bob
}
```

**kubeagent example:** collecting findings:

```go
var findings []Finding
for _, f := range facts {          // f is each PodFacts
    for _, d := range detectors {  // d is each Detector
        if finding := d.Detect(f); finding != nil {
            findings = append(findings, *finding)
        }
    }
}
```

---

## 8. Multiple return values and error handling

Go functions can return more than one value. The idiom is to return
`(result, error)`. There are **no exceptions** — you check the error explicitly,
right where it can happen. `nil` error means "all good."

**Simple example:**

```go
func half(n int) (int, error) {
    if n%2 != 0 {
        return 0, fmt.Errorf("%d is odd, can't halve cleanly", n)
    }
    return n / 2, nil
}

result, err := half(7)
if err != nil {
    fmt.Println("error:", err)   // error: 7 is odd, can't halve cleanly
    return
}
fmt.Println(result)
```

**kubeagent example:** connecting to the cluster can fail, so we check and add
context with `%w` (which wraps the original error so the full chain shows):

```go
clientset, err := cluster.NewClient(kubeconfigPath)
if err != nil {
    return fmt.Errorf("connecting to cluster: %w", err)
}
```

---

## 9. Tests (the `testing` package)

A test is a function named `TestXxx(t *testing.T)` in a `_test.go` file. Feed
input, check output, call `t.Error`/`t.Fatal` if it's wrong. Run with
`go test ./...`.

**Simple example:**

```go
func TestHalf(t *testing.T) {
    got, err := half(8)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if got != 4 {
        t.Errorf("half(8) = %d, want 4", got)
    }
}
```

**kubeagent example:** because a detector is just "facts in → finding out," we
test it with a fake pod — no cluster needed:

```go
func TestCrashLoopDetector(t *testing.T) {
    facts := PodFacts{Pod: fakePodInState("CrashLoopBackOff")}
    if CrashLoopDetector{}.Detect(facts) == nil {
        t.Fatal("expected a CrashLoopBackOff finding")
    }
}
```

---

## 10. JSON encoding (`encoding/json`)

Go turns structs into JSON (and back) with the `encoding/json` package. Struct
field tags like `` `json:"pod"` `` control the JSON key names.

**Simple example:**

```go
type User struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}

b, _ := json.Marshal(User{Name: "ann", Age: 30}) // {"name":"ann","age":30}
```

**kubeagent example:** `Finding` carries JSON tags, so `--output json` emits a
clean array:

```go
enc := json.NewEncoder(w)
enc.SetIndent("", "  ")
enc.Encode(findings)
```

---

## Coming later

These will be added to this file when we reach them:

- **Goroutines & channels** — Go's lightweight concurrency, for fetching pod
  facts in parallel.
- **Working with the `client-go` typed API** — clientsets, list options, and the
  big Kubernetes object model.
