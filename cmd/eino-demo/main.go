package main

import (
	"context"
	"fmt"
	"log"

	"agent-project/internal/agent"
)

func main() {
	pipeline, err := agent.NewPipeline(context.Background(), agent.NewDemoChatModel())
	if err != nil {
		log.Fatal(err)
	}

	input := agent.Input{Query: "如何定位订单服务错误？"}
	chainOutput, err := pipeline.RunChain(context.Background(), input)
	if err != nil {
		log.Fatal(err)
	}
	graphOutput, err := pipeline.RunGraph(context.Background(), input)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("chain: %s\n", chainOutput.Answer)
	fmt.Printf("graph: %s\n", graphOutput.Answer)
}
