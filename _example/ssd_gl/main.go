package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	"github.com/mattn/go-tflite"
	"github.com/mattn/go-tflite/delegates/gpu/gl"
	builtinOps "github.com/mattn/go-tflite/ops"

	"gocv.io/x/gocv"
	"golang.org/x/image/colornames"
)

var (
	video     = flag.String("camera", "0", "video cature")
	modelPath = flag.String("model", "detect.tflite", "path to model file")
	labelPath = flag.String("label", "labelmap.txt", "path to label file")
)

type ssdResult struct {
	loc   [][4]float32
	clazz []float32
	score []float32
	mat   gocv.Mat
}

type ssdClass struct {
	loc   [4]float32
	score float64
	index int
}

type result interface {
	Image() image.Image
}

func loadLabels(filename string) ([]string, error) {
	labels := []string{}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		labels = append(labels, scanner.Text())
	}
	return labels, nil
}

func detect(ctx context.Context, wg *sync.WaitGroup, resultChan chan<- *ssdResult, interpreter *tflite.Interpreter, wanted_width, wanted_height, wanted_channels int, cam *gocv.VideoCapture) {
	defer wg.Done()
	defer close(resultChan)

	input := interpreter.GetInputTensor(0)
	qp := input.QuantizationParams()
	log.Printf("width: %v, height: %v, type: %v, scale: %v, zeropoint: %v", wanted_width, wanted_height, input.Type(), qp.Scale, qp.ZeroPoint)
	log.Printf("input tensor count: %v, output tensor count: %v", interpreter.GetInputTensorCount(), interpreter.GetOutputTensorCount())
	if qp.Scale == 0 {
		qp.Scale = 1
	}

	var loc [1][20][4]float32
	var clazz [1][20]float32
	var score [1][20]float32
	var nums [1]float32

	output1 := interpreter.GetOutputTensor(0)
	output2 := interpreter.GetOutputTensor(1)
	output3 := interpreter.GetOutputTensor(2)
	output4 := interpreter.GetOutputTensor(3)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if len(resultChan) == cap(resultChan) {
			continue
		}

		frame := gocv.NewMat()
		if ok := cam.Read(&frame); !ok {
			frame.Close()
			break
		}

		resized := gocv.NewMat()
		gocv.Resize(frame, &resized, image.Pt(wanted_width, wanted_height), 0, 0, gocv.InterpolationDefault)
		if v, err := resized.DataPtrUint8(); err == nil {
			copy(input.UInt8s(), v)
		}
		resized.Close()
		status := interpreter.Invoke()
		if status != tflite.OK {
			log.Println("invoke failed")
			return
		}

		output1.CopyToBuffer(&loc[0])
		output2.CopyToBuffer(&clazz[0])
		output3.CopyToBuffer(&score[0])
		output4.CopyToBuffer(&nums[0])
		num := int(nums[0])

		resultChan <- &ssdResult{
			loc:   loc[0][:num],
			clazz: clazz[0][:num],
			score: score[0][:num],
			mat:   frame,
		}
	}
}

func main() {
	flag.Parse()

	labels, err := loadLabels(*labelPath)
	if err != nil {
		log.Fatal(err)
	}

	// Setup Pixel window
	window := gocv.NewWindow("Webcam Window")
	defer window.Close()

	ctx, cancel := context.WithCancel(context.Background())

	cam, err := gocv.OpenVideoCapture(*video)
	if err != nil {
		log.Printf("cannot open camera: %v", err)
		return
	}
	defer cam.Close()

	window.ResizeWindow(
		int(cam.Get(gocv.VideoCaptureFrameWidth)),
		int(cam.Get(gocv.VideoCaptureFrameHeight)),
	)

	model := tflite.NewModelFromFile(*modelPath)
	if model == nil {
		log.Println("cannot load model")
		return
	}
	//defer model.Delete()

	// Get the list of devices
	options := tflite.NewInterpreterOptions()
	options.SetNumThread(4)
	options.ExpAddBuiltinOp(tflite.BuiltinOperator_CONCATENATION, builtinOps.Register_CONCATENATION_REF(), 1, 1)
	options.ExpAddBuiltinOp(tflite.BuiltinOperator_CONV_2D, builtinOps.Register_CONV_2D(), 1, 1)
	options.ExpAddBuiltinOp(tflite.BuiltinOperator_LOGISTIC, builtinOps.Register_LOGISTIC_REF(), 1, 1)
	options.ExpAddBuiltinOp(tflite.BuiltinOperator_RESHAPE, builtinOps.Register_RESHAPE(), 1, 1)
	options.ExpAddBuiltinOp(tflite.BuiltinOperator_DEPTHWISE_CONV_2D, builtinOps.Register_DEPTHWISE_CONV_2D(), 1, 1)
	options.AddDelegate(gl.New(nil))

	defer options.Delete()

	interpreter := tflite.NewInterpreter(model, options)
	if interpreter == nil {
		log.Println("cannot create interpreter")
		return
	}
	defer interpreter.Delete()

	status := interpreter.AllocateTensors()
	if status != tflite.OK {
		log.Println("allocate failed")
		return
	}

	input := interpreter.GetInputTensor(0)
	wanted_height := input.Dim(1)
	wanted_width := input.Dim(2)
	wanted_channels := input.Dim(3)

	var wg sync.WaitGroup
	wg.Add(1)

	// Start up the background capture
	resultChan := make(chan *ssdResult, 1)
	go detect(ctx, &wg, resultChan, interpreter, wanted_width, wanted_height, wanted_channels, cam)

	sc := make(chan os.Signal, 1)
	defer close(sc)
	signal.Notify(sc, os.Interrupt)
	go func() {
		<-sc
		cancel()
	}()

	// Some local vars to calculate frame rate
	var (
		frames = 0
		second = time.Tick(time.Second)
	)

	for {
		// Run inference if we have a new frame to read
		result, ok := <-resultChan
		if !ok {
			break
		}

		classes := make([]ssdClass, 0, len(result.clazz))
		var i int
		for i = 0; i < len(result.clazz); i++ {
			score := float64(result.score[i])
			if score < 0.6 {
				continue
			}
			classes = append(classes, ssdClass{loc: result.loc[i], score: score, index: int(result.clazz[i])})
		}
		sort.Slice(classes, func(i, j int) bool {
			return classes[i].score > classes[j].score
		})
		if len(classes) > 5 {
			classes = classes[:5]
		}

		size := result.mat.Size()
		for i, class := range classes {
			label := "unknown"
			if class.index < len(labels) {
				label = labels[class.index]
			}
			c := colornames.Map[colornames.Names[class.index%len(colornames.Names)]]
			gocv.Rectangle(&result.mat, image.Rect(
				int(float32(size[1])*class.loc[1]),
				int(float32(size[0])*class.loc[0]),
				int(float32(size[1])*class.loc[3]),
				int(float32(size[0])*class.loc[2]),
			), c, 2)
			text := fmt.Sprintf("%d %.5f %s", i, class.score, label)
			gocv.PutText(&result.mat, text, image.Pt(
				int(float32(size[1])*class.loc[1]),
				int(float32(size[0])*class.loc[0]),
			), gocv.FontHersheySimplex, 1.2, c, 1)
		}

		window.IMShow(result.mat)
		result.mat.Close()

		k := window.WaitKey(10)
		if k == 0x1b {
			break
		}
		if window.GetWindowProperty(gocv.WindowPropertyVisible) == 0 {
			break
		}

		// calculate frame rate
		frames++
		select {
		case <-second:
			window.SetWindowTitle(fmt.Sprintf("SSD | FPS: %d", frames))
			frames = 0
		default:
		}
	}

	cancel()
	wg.Wait()
}
