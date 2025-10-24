package main

import (
    "flag"
    "fmt"
    "os"
)

func main() {
    // Define CLI flags
    name := flag.String("name", "Guest", "Your name")
    lang := flag.String("lang", "en", "Language (en/es/fr)")

    // Parse the flags
    flag.Parse()

    // If user provides 'help'
    if len(os.Args) == 1 {
        fmt.Println("Usage: go run main.go -name=<your_name> -lang=<language>")
        fmt.Println("Example: go run main.go -name=Ayush -lang=fr")
        return
    }

    // Output greeting based on language
    switch *lang {
    case "en":
        fmt.Printf("Hello, %s! \n", *name)
    case "es":
        fmt.Printf("¡Hola, %s! \n", *name)
    case "fr":
        fmt.Printf("Bonjour, %s! \n", *name)
    default:
        fmt.Printf("Hello, %s! (Language not recognized)\n", *name)
    }
}
