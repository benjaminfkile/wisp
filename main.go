package main

import "fmt"

// greeting is factored out of main so it can be unit-tested.
func greeting() string {
	return "Hello from Wisp!"
}

func main() {
	fmt.Println(greeting())
}
