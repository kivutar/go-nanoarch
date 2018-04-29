package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/mobile/exp/audio/al"

	"github.com/go-gl/gl/v4.1-core/gl"
	"github.com/go-gl/glfw/v3.2/glfw"
)

/*
#include "libretro.h"
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include <stdio.h>
#include <dlfcn.h>
#include <string.h>

void bridge_retro_init(void *f);
void bridge_retro_deinit(void *f);
unsigned bridge_retro_api_version(void *f);
void bridge_retro_get_system_info(void *f, struct retro_system_info *si);
void bridge_retro_get_system_av_info(void *f, struct retro_system_av_info *si);
bool bridge_retro_set_environment(void *f, void *callback);
void bridge_retro_set_video_refresh(void *f, void *callback);
void bridge_retro_set_input_poll(void *f, void *callback);
void bridge_retro_set_input_state(void *f, void *callback);
void bridge_retro_set_audio_sample(void *f, void *callback);
void bridge_retro_set_audio_sample_batch(void *f, void *callback);
bool bridge_retro_load_game(void *f, struct retro_game_info *gi);
void bridge_retro_unload_game(void *f);
void bridge_retro_run(void *f);

bool coreEnvironment_cgo(unsigned cmd, void *data);
void coreVideoRefresh_cgo(void *data, unsigned width, unsigned height, size_t pitch);
void coreInputPoll_cgo();
void coreAudioSample_cgo(int16_t left, int16_t right);
size_t coreAudioSampleBatch_cgo(const int16_t *data, size_t frames);
int16_t coreInputState_cgo(unsigned port, unsigned device, unsigned index, unsigned id);
void coreLog_cgo(enum retro_log_level level, const char *msg);
*/
import "C"

var window *glfw.Window

var mu sync.Mutex

var video struct {
	program uint32
	vao     uint32
	texID   uint32
	pitch   uint32
	pixFmt  uint32
	pixType uint32
	bpp     uint32
}

var scale = 3.0

const bufSize = 1024 * 4

var audio struct {
	source     al.Source
	buffers    []al.Buffer
	rate       int32
	numBuffers int32
	tmpBuf     [bufSize]byte
	tmpBufPtr  C.size_t
	bufPtr     C.size_t
	resPtr     int32
}

var binds = map[glfw.Key]C.int{
	glfw.KeyX:         C.RETRO_DEVICE_ID_JOYPAD_A,
	glfw.KeyZ:         C.RETRO_DEVICE_ID_JOYPAD_B,
	glfw.KeyA:         C.RETRO_DEVICE_ID_JOYPAD_Y,
	glfw.KeyS:         C.RETRO_DEVICE_ID_JOYPAD_X,
	glfw.KeyUp:        C.RETRO_DEVICE_ID_JOYPAD_UP,
	glfw.KeyDown:      C.RETRO_DEVICE_ID_JOYPAD_DOWN,
	glfw.KeyLeft:      C.RETRO_DEVICE_ID_JOYPAD_LEFT,
	glfw.KeyRight:     C.RETRO_DEVICE_ID_JOYPAD_RIGHT,
	glfw.KeyEnter:     C.RETRO_DEVICE_ID_JOYPAD_START,
	glfw.KeyBackspace: C.RETRO_DEVICE_ID_JOYPAD_SELECT,
}

var joy [C.RETRO_DEVICE_ID_JOYPAD_R3 + 1]bool

func videoSetPixelFormat(format uint32) C.bool {
	fmt.Printf("videoSetPixelFormat: %v\n", format)
	if video.texID != 0 {
		log.Fatal("Tried to change pixel format after initialization.")
	}

	switch format {
	case C.RETRO_PIXEL_FORMAT_0RGB1555:
		video.pixFmt = gl.UNSIGNED_SHORT_5_5_5_1
		video.pixType = gl.BGRA
		video.bpp = 2
		break
	case C.RETRO_PIXEL_FORMAT_XRGB8888:
		video.pixFmt = gl.UNSIGNED_INT_8_8_8_8_REV
		video.pixType = gl.BGRA
		video.bpp = 4
		break
	case C.RETRO_PIXEL_FORMAT_RGB565:
		video.pixFmt = gl.UNSIGNED_SHORT_5_6_5
		video.pixType = gl.RGB
		video.bpp = 2
		break
	default:
		log.Fatalf("Unknown pixel type %v", format)
	}

	return true
}

/**
 * When resizing the window, resize the content.
 */
func resizedFramebuffer(w *glfw.Window, width int, height int) {
	gl.Viewport(0, 0, int32(width), int32(height))
}

func createWindow(width int, height int) {
	glfw.WindowHint(glfw.Resizable, glfw.False)
	glfw.WindowHint(glfw.ContextVersionMajor, 4)
	glfw.WindowHint(glfw.ContextVersionMinor, 1)
	glfw.WindowHint(glfw.Resizable, 1)
	glfw.WindowHint(glfw.OpenGLProfile, glfw.OpenGLCoreProfile)
	glfw.WindowHint(glfw.OpenGLForwardCompatible, glfw.True)
	var err error
	window, err = glfw.CreateWindow(width, height, "nanorarch", nil, nil)
	if err != nil {
		panic(err)
	}

	window.MakeContextCurrent()

	// Force the same aspect ratio.
	window.SetAspectRatio(width, height)

	// When resizing the window, also resize the content.
	window.SetFramebufferSizeCallback(resizedFramebuffer)

	// Initialize Glow
	if err := gl.Init(); err != nil {
		panic(err)
	}

	version := gl.GoStr(gl.GetString(gl.VERSION))
	fmt.Println("OpenGL version", version)

	// Configure the vertex and fragment shaders
	video.program, err = newProgram(vertexShader, fragmentShader)
	if err != nil {
		panic(err)
	}

	gl.UseProgram(video.program)

	textureUniform := gl.GetUniformLocation(video.program, gl.Str("tex\x00"))
	gl.Uniform1i(textureUniform, 0)

	gl.BindFragDataLocation(video.program, 0, gl.Str("outputColor\x00"))

	// Configure the vertex data
	gl.GenVertexArrays(1, &video.vao)
	gl.BindVertexArray(video.vao)

	var vbo uint32
	gl.GenBuffers(1, &vbo)
	gl.BindBuffer(gl.ARRAY_BUFFER, vbo)
	gl.BufferData(gl.ARRAY_BUFFER, len(vertices)*4, gl.Ptr(vertices), gl.STATIC_DRAW)

	vertAttrib := uint32(gl.GetAttribLocation(video.program, gl.Str("vert\x00")))
	gl.EnableVertexAttribArray(vertAttrib)
	gl.VertexAttribPointer(vertAttrib, 2, gl.FLOAT, false, 4*4, gl.PtrOffset(0))

	texCoordAttrib := uint32(gl.GetAttribLocation(video.program, gl.Str("vertTexCoord\x00")))
	gl.EnableVertexAttribArray(texCoordAttrib)
	gl.VertexAttribPointer(texCoordAttrib, 2, gl.FLOAT, false, 4*4, gl.PtrOffset(2*4))
}

func resizeToAspect(ratio float64, sw float64, sh float64) (dw float64, dh float64) {
	if ratio <= 0 {
		ratio = sw / sh
	}

	if sw/sh < 1.0 {
		dw = dh * ratio
		dh = sh
	} else {
		dw = sw
		dh = dw / ratio
	}
	return
}

func videoConfigure(geom *C.struct_retro_game_geometry) {

	nwidth, nheight := resizeToAspect(float64(geom.aspect_ratio), float64(geom.base_width), float64(geom.base_height))

	nwidth = nwidth * scale
	nheight = nheight * scale

	if window == nil {
		createWindow(int(nwidth), int(nheight))
	}

	if video.pixFmt == 0 {
		video.pixFmt = gl.UNSIGNED_SHORT_5_5_5_1
	}

	gl.GenTextures(1, &video.texID)

	gl.ActiveTexture(gl.TEXTURE0)
	if video.texID == 0 {
		fmt.Println("Failed to create the video texture")
	}

	video.pitch = uint32(geom.base_width) * video.bpp

	gl.BindTexture(gl.TEXTURE_2D, video.texID)

	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST)
	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST)
}

//export coreVideoRefresh
func coreVideoRefresh(data unsafe.Pointer, width C.unsigned, height C.unsigned, pitch C.size_t) {
	gl.TexImage2D(gl.TEXTURE_2D, 0, gl.RGBA8, int32(width), int32(height), 0, video.pixType, video.pixFmt, nil)

	gl.BindTexture(gl.TEXTURE_2D, video.texID)

	if uint32(pitch) != video.pitch {
		video.pitch = uint32(pitch)
		gl.PixelStorei(gl.UNPACK_ROW_LENGTH, int32(video.pitch/video.bpp))
	}

	if data != nil {
		gl.TexSubImage2D(gl.TEXTURE_2D, 0, 0, 0, int32(width), int32(height), video.pixType, video.pixFmt, data)
	}
}

//export coreInputPoll
func coreInputPoll() {
	for k, v := range binds {
		joy[v] = (window.GetKey(k) == glfw.Press)
	}

	// Close the window when the user hits the Escape key.
	if window.GetKey(glfw.KeyEscape) == glfw.Press {
		window.SetShouldClose(true)
	}
}

//export coreInputState
func coreInputState(port C.unsigned, device C.unsigned, index C.unsigned, id C.unsigned) C.int16_t {
	if port > 0 || index > 0 || device != C.RETRO_DEVICE_JOYPAD {
		return 0
	}

	if id < 255 && joy[id] {
		return 1
	}
	return 0
}

func audioInit(rate C.double) {
	err := al.OpenDevice()
	if err != nil {
		fmt.Println(err)
	}

	audio.rate = int32(rate)
	audio.numBuffers = 4

	fmt.Printf("[OpenAL]: Using %v buffers of %v bytes.\n", audio.numBuffers, bufSize)

	audio.source = al.GenSources(1)[0]
	audio.buffers = al.GenBuffers(int(audio.numBuffers))
	audio.resPtr = audio.numBuffers

	if retroAudioSetStateCallback {
		retroAudioSetStateCallback(true)
	}
}

func min(a, b C.size_t) C.size_t {
	if a < b {
		return a
	}
	return b
}

func alUnqueueBuffers() bool {
	val := audio.source.BuffersProcessed()

	if val <= 0 {
		return false
	}

	audio.source.UnqueueBuffers(audio.buffers[audio.resPtr:val]...)
	audio.resPtr += val
	return true
}

func alGetBuffer() (al.Buffer, error) {
	if audio.resPtr == 0 {
		for {
			if alUnqueueBuffers() {
				break
			}

			// if audio.nonblock
			//   return nil, true

			/* Must sleep as there is no proper blocking method. */
			time.Sleep(time.Millisecond)
		}
	}

	audio.resPtr--
	return audio.buffers[audio.resPtr], nil
}

func fillInternalBuf(buf unsafe.Pointer, size C.size_t) C.size_t {
	readSize := min(bufSize-audio.tmpBufPtr, size)
	//memcpy(audio.tmpBuf+audio.tmpBufPtr, buf, readSize)
	copy(audio.tmpBuf[audio.tmpBufPtr:], C.GoBytes(buf, bufSize)[audio.bufPtr:audio.bufPtr+readSize])
	audio.tmpBufPtr += readSize
	return readSize
}

func audioWrite(buf unsafe.Pointer, size C.size_t) C.size_t {
	written := C.size_t(0)

	for size > 0 {

		rc := fillInternalBuf(buf, size)

		written += rc
		audio.bufPtr += rc
		size -= rc

		if audio.tmpBufPtr != bufSize {
			break
		}

		buffer, err := alGetBuffer()
		if err != nil {
			break
		}

		buffer.BufferData(al.FormatStereo16, audio.tmpBuf[:], audio.rate)
		audio.tmpBufPtr = 0
		audio.source.QueueBuffers(buffer)

		if audio.source.State() != al.Playing {
			al.PlaySources(audio.source)
		}
	}

	audio.bufPtr = 0

	return written
}

//export coreAudioSample
func coreAudioSample(left C.int16_t, right C.int16_t) {
	buf := []C.int16_t{left, right}
	audioWrite(unsafe.Pointer(&buf), 4)
}

//export coreAudioSampleBatch
func coreAudioSampleBatch(data unsafe.Pointer, frames C.size_t) C.size_t {
	return audioWrite(data, frames*4)
}

//export coreLog
func coreLog(level C.enum_retro_log_level, msg *C.char) {
	fmt.Print("[Log]: ", C.GoString(msg))
}

//export coreEnvironment
func coreEnvironment(cmd C.unsigned, data unsafe.Pointer) C.bool {
	switch cmd {
	case C.RETRO_ENVIRONMENT_GET_USERNAME:
		username := (**C.char)(data)
		currentUser, err := user.Current()
		if err != nil {
			*username = C.CString("")
		} else {
			*username = C.CString(currentUser.Username)
		}
		break
	case C.RETRO_ENVIRONMENT_GET_LOG_INTERFACE:
		cb := (*C.struct_retro_log_callback)(data)
		cb.log = (C.retro_log_printf_t)(C.coreLog_cgo)
		break
	case C.RETRO_ENVIRONMENT_GET_CAN_DUPE:
		bval := (*C.bool)(data)
		*bval = C.bool(true)
		break
	case C.RETRO_ENVIRONMENT_SET_AUDIO_CALLBACK:
		audio_cb := (*C.struct_retro_audio_callback)(data)
		retroAudioCallback = audio_cb.callback
		retroAudioSetStateCallback = audio_cb.set_state
		break
	case C.RETRO_ENVIRONMENT_SET_PIXEL_FORMAT:
		format := (*C.enum_retro_pixel_format)(data)
		if *format > C.RETRO_PIXEL_FORMAT_RGB565 {
			return false
		}
		return videoSetPixelFormat(*format)
	case C.RETRO_ENVIRONMENT_GET_SYSTEM_DIRECTORY:
	case C.RETRO_ENVIRONMENT_GET_SAVE_DIRECTORY:
		path := (**C.char)(data)
		*path = C.CString(".")
		return true
	case C.RETRO_ENVIRONMENT_SHUTDOWN:
		window.SetShouldClose(true)
		return true
	case C.RETRO_ENVIRONMENT_GET_VARIABLE:
		variable := (*C.struct_retro_variable)(data)
		fmt.Println("[Env]: get variable:", C.GoString(variable.key))
		return false
	default:
		//fmt.Println("[Env]: command not implemented", cmd)
		return false
	}
	return true
}

func init() {
	// GLFW event handling must run on the main OS thread
	runtime.LockOSThread()
}

var retroInit unsafe.Pointer
var retroDeinit unsafe.Pointer
var retroAPIVersion unsafe.Pointer
var retroGetSystemInfo unsafe.Pointer
var retroGetSystemAVInfo unsafe.Pointer
var retroSetEnvironment unsafe.Pointer
var retroSetVideoRefresh unsafe.Pointer
var retroSetInputPoll unsafe.Pointer
var retroSetInputState unsafe.Pointer
var retroAudioCallback unsafe.Pointer
var retroAudioSetStateCallback unsafe.Pointer
var retroSetAudioSample unsafe.Pointer
var retroSetAudioSampleBatch unsafe.Pointer
var retroRun unsafe.Pointer
var retroLoadGame unsafe.Pointer
var retroUnloadGame unsafe.Pointer

func coreLoad(sofile string) {

	mu.Lock()
	h := C.dlopen(C.CString(sofile), C.RTLD_NOW)
	if h == nil {
		log.Fatalf("error loading %s\n", sofile)
	}

	retroInit = C.dlsym(h, C.CString("retro_init"))
	retroDeinit = C.dlsym(h, C.CString("retro_deinit"))
	retroAPIVersion = C.dlsym(h, C.CString("retro_api_version"))
	retroGetSystemInfo = C.dlsym(h, C.CString("retro_get_system_info"))
	retroGetSystemAVInfo = C.dlsym(h, C.CString("retro_get_system_av_info"))
	retroSetEnvironment = C.dlsym(h, C.CString("retro_set_environment"))
	retroSetVideoRefresh = C.dlsym(h, C.CString("retro_set_video_refresh"))
	retroSetInputPoll = C.dlsym(h, C.CString("retro_set_input_poll"))
	retroSetInputState = C.dlsym(h, C.CString("retro_set_input_state"))
	retroSetAudioSample = C.dlsym(h, C.CString("retro_set_audio_sample"))
	retroSetAudioSampleBatch = C.dlsym(h, C.CString("retro_set_audio_sample_batch"))
	retroRun = C.dlsym(h, C.CString("retro_run"))
	retroLoadGame = C.dlsym(h, C.CString("retro_load_game"))
	retroUnloadGame = C.dlsym(h, C.CString("retro_unload_game"))
	mu.Unlock()

	C.bridge_retro_set_environment(retroSetEnvironment, C.coreEnvironment_cgo)
	C.bridge_retro_set_video_refresh(retroSetVideoRefresh, C.coreVideoRefresh_cgo)
	C.bridge_retro_set_input_poll(retroSetInputPoll, C.coreInputPoll_cgo)
	C.bridge_retro_set_input_state(retroSetInputState, C.coreInputState_cgo)
	C.bridge_retro_set_audio_sample(retroSetAudioSample, C.coreAudioSample_cgo)
	C.bridge_retro_set_audio_sample_batch(retroSetAudioSampleBatch, C.coreAudioSampleBatch_cgo)

	C.bridge_retro_init(retroInit)

	v := C.bridge_retro_api_version(retroAPIVersion)
	fmt.Println("Libretro API version:", v)
}

func slurp(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bytes := make([]byte, size)
	buffer := bufio.NewReader(f)
	_, err = buffer.Read(bytes)
	if err != nil {
		return nil, err
	}
	return bytes, nil
}

func coreLoadGame(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		panic(err)
	}

	fi, err := file.Stat()
	if err != nil {
		panic(err)
	}

	size := fi.Size()

	fmt.Println("ROM size:", size)

	gi := C.struct_retro_game_info{
		path: C.CString(filename),
		size: C.size_t(size),
	}

	si := C.struct_retro_system_info{}

	C.bridge_retro_get_system_info(retroGetSystemInfo, &si)

	var libName = C.GoString(si.library_name)
	fmt.Println("  library_name:", libName)
	fmt.Println("  library_version:", C.GoString(si.library_version))
	fmt.Println("  valid_extensions:", C.GoString(si.valid_extensions))
	fmt.Println("  need_fullpath:", si.need_fullpath)
	fmt.Println("  block_extract:", si.block_extract)

	if !si.need_fullpath {
		bytes, err := slurp(filename, size)
		if err != nil {
			panic(err)
		}
		cstr := C.CString(string(bytes))
		gi.data = unsafe.Pointer(cstr)

	}

	ok := C.bridge_retro_load_game(retroLoadGame, &gi)
	if !ok {
		log.Fatal("The core failed to load the content.")
	}

	avi := C.struct_retro_system_av_info{}

	C.bridge_retro_get_system_av_info(retroGetSystemAVInfo, &avi)

	videoConfigure(&avi.geometry)
	// Append the library name to the window title.
	if len(libName) > 0 {
		window.SetTitle("nanoarch - " + libName)
	}
	audioInit(avi.timing.sample_rate)
}

func videoRender() {
	gl.BindVertexArray(video.vao)

	gl.ActiveTexture(gl.TEXTURE0)
	gl.BindTexture(gl.TEXTURE_2D, video.texID)

	gl.DrawArrays(gl.TRIANGLE_STRIP, 0, 4)
}

func main() {
	var corePath = flag.String("L", "", "Path to the libretro core")
	var gamePath = flag.String("G", "", "Path to the game")
	flag.Parse()

	if err := glfw.Init(); err != nil {
		log.Fatalln("failed to initialize glfw:", err)
	}
	defer glfw.Terminate()

	coreLoad(*corePath)
	coreLoadGame(*gamePath)

	for !window.ShouldClose() {
		// Ask the core to emit audio.
		if retroAudioCallback {
			retroAudioCallback()
		}

		glfw.PollEvents()

		C.bridge_retro_run(retroRun)

		gl.Clear(gl.COLOR_BUFFER_BIT)

		videoRender()

		window.SwapBuffers()
	}

	// Unload and deinit in the core.
	C.bridge_retro_unload_game(retroUnloadGame)
	C.bridge_retro_deinit(retroDeinit)
}

func newProgram(vertexShaderSource, fragmentShaderSource string) (uint32, error) {
	vertexShader, err := compileShader(vertexShaderSource, gl.VERTEX_SHADER)
	if err != nil {
		return 0, err
	}

	fragmentShader, err := compileShader(fragmentShaderSource, gl.FRAGMENT_SHADER)
	if err != nil {
		return 0, err
	}

	program := gl.CreateProgram()

	gl.AttachShader(program, vertexShader)
	gl.AttachShader(program, fragmentShader)
	gl.LinkProgram(program)

	var status int32
	gl.GetProgramiv(program, gl.LINK_STATUS, &status)
	if status == gl.FALSE {
		var logLength int32
		gl.GetProgramiv(program, gl.INFO_LOG_LENGTH, &logLength)

		log := strings.Repeat("\x00", int(logLength+1))
		gl.GetProgramInfoLog(program, logLength, nil, gl.Str(log))

		return 0, fmt.Errorf("failed to link program: %v", log)
	}

	gl.DeleteShader(vertexShader)
	gl.DeleteShader(fragmentShader)

	return program, nil
}

func compileShader(source string, shaderType uint32) (uint32, error) {
	shader := gl.CreateShader(shaderType)

	csources, free := gl.Strs(source)
	gl.ShaderSource(shader, 1, csources, nil)
	free()
	gl.CompileShader(shader)

	var status int32
	gl.GetShaderiv(shader, gl.COMPILE_STATUS, &status)
	if status == gl.FALSE {
		var logLength int32
		gl.GetShaderiv(shader, gl.INFO_LOG_LENGTH, &logLength)

		log := strings.Repeat("\x00", int(logLength+1))
		gl.GetShaderInfoLog(shader, logLength, nil, gl.Str(log))

		return 0, fmt.Errorf("failed to compile %v: %v", source, log)
	}

	return shader, nil
}

var vertexShader = `
#version 330

in vec2 vert;
in vec2 vertTexCoord;

out vec2 fragTexCoord;

void main() {
    fragTexCoord = vertTexCoord;
    gl_Position = vec4(vert, 0, 1);
}
` + "\x00"

var fragmentShader = `
#version 330

uniform sampler2D tex;

in vec2 fragTexCoord;

out vec4 outputColor;

void main() {
    outputColor = texture(tex, fragTexCoord);
}
` + "\x00"

var vertices = []float32{
	//  X, Y, U, V
	-1.0, -1.0, 0.0, 1.0, // left-bottom
	-1.0, 1.0, 0.0, 0.0, // left-top
	1.0, -1.0, 1.0, 1.0, // right-bottom
	1.0, 1.0, 1.0, 0.0, // right-top
}
