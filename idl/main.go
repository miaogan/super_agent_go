package main

import (
	agentv1 "github.com/aegis/idl/kitex_gen/agentv1/agentservice"
	"log"
)

func main() {
	svr := agentv1.NewServer(new(AgentServiceImpl))

	err := svr.Run()

	if err != nil {
		log.Println(err.Error())
	}
}
