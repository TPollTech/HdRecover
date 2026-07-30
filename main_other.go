//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("HdRecover: a interface gráfica está disponível somente para Windows 10/11.")
}
