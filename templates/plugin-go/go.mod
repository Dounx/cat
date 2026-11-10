module example.com/cat-plugin-hello

go 1.24

require github.com/dounx/cat/sdk/go v0.0.0

// While the SDK is not published, point at a checkout of the Cat repository.
replace github.com/dounx/cat/sdk/go => ../../sdk/go
