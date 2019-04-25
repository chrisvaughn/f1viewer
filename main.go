package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell"
	"github.com/rivo/tview"
)

type config struct {
	Lang                  string    `json:"preferred_language"`
	CheckUpdate           bool      `json:"check_updates"`
	CustomPlaybackOptions []command `json:"custom_playback_options"`
}

type command struct {
	Title          string     `json:"title"`
	Concurrent     bool       `json:"concurrent"`
	Commands       [][]string `json:"commands"`
	Watchphrase    string     `json:"watchphrase"`
	CommandToWatch int        `json:"command_to_watch"`
}

type nodeContext struct {
	EpID          string
	CustomOptions command
	Title         string
}

var vodTypes vodTypesStruct
var con config
var abortWritingInfo chan bool
var episodeMap map[string]episodeStruct
var driverMap map[string]driverStruct
var teamMap map[string]teamStruct

var episodeMapMutex = sync.RWMutex{}
var driverMapMutex = sync.RWMutex{}
var teamMapMutex = sync.RWMutex{}

var app *tview.Application
var infoTable *tview.Table
var debugText *tview.TextView
var tree *tview.TreeView

func main() {
	//start UI
	app = tview.NewApplication()
	file, err := ioutil.ReadFile("config.json")
	con.CheckUpdate = true
	con.Lang = "en"
	if err != nil {
		debugPrint(err.Error())
	} else {
		err = json.Unmarshal(file, &con)
		if err != nil {
			debugPrint("malformed configuration file:")
			debugPrint(err.Error())
		}
	}
	abortWritingInfo = make(chan bool)
	//cache
	episodeMap = make(map[string]episodeStruct)
	driverMap = make(map[string]driverStruct)
	teamMap = make(map[string]teamStruct)
	//build base tree
	root := tview.NewTreeNode("VOD-Types").
		SetColor(tcell.ColorBlue).
		SetSelectable(false)
	tree = tview.NewTreeView().
		SetRoot(root).
		SetCurrentNode(root)
	var allSeasons allSeasonStruct
	//check for live session
	go func() {
		if isLive, liveNode := getLiveNode(); isLive {
			insertNodeAtTop(root, liveNode)
			app.Draw()
		}
	}()
	fullSessions := tview.NewTreeNode("Full Race Weekends").
		SetSelectable(true).
		SetReference(allSeasons).
		SetColor(tcell.ColorYellow)
	root.AddChild(fullSessions)
	go func() {
		vodTypes = getVodTypes()
		for i, vType := range vodTypes.Objects {
			if len(vType.ContentUrls) > 0 {
				node := tview.NewTreeNode(vType.Name).
					SetSelectable(true).
					SetReference(i).
					SetColor(tcell.ColorYellow)
				root.AddChild(node)
			}
		}
		app.Draw()
	}()
	//check if an update is available
	if con.CheckUpdate {
		go func() {
			node, err := getUpdateNode()
			if err != nil {
				debugPrint(err.Error())
			} else {
				insertNodeAtTop(root, node)
				app.Draw()
			}
		}()
	}
	//display info for the episode or VOD type the cursor is on
	tree.SetChangedFunc(switchNode)
	//what happens when a node is selected
	tree.SetSelectedFunc(nodeSelected)
	//flex containing everything
	flex := tview.NewFlex()
	//flex containing metadata and debug
	rowFlex := tview.NewFlex()
	rowFlex.SetDirection(tview.FlexRow)
	//metadata window
	infoTable = tview.NewTable()
	infoTable.SetBorder(true).SetTitle(" Info ")
	//debug window
	debugText = tview.NewTextView()
	debugText.SetBorder(true).SetTitle("Debug")
	debugText.SetChangedFunc(func() {
		app.Draw()
	})

	flex.AddItem(tree, 0, 2, true)
	flex.AddItem(rowFlex, 0, 2, false)
	rowFlex.AddItem(infoTable, 0, 2, false)
	//flag -d enables debug window
	if checkArgs("-d") {
		rowFlex.AddItem(debugText, 0, 1, false)
	}
	app.SetRoot(flex, true).Run()
}

//takes struct reflect Types and values and draws them as a table
func getTableValuesFromInterface(stru interface{}) ([]string, [][]string) {
	titles := reflect.TypeOf(stru)
	values := reflect.ValueOf(stru)
	t := make([]string, 1)
	v := make([][]string, 1)

	//iterate through titles and values and add them to the slices
	for i := 0; i < titles.NumField(); i++ {
		title := titles.Field(i)
		value := values.Field(i)

		if value.Kind() == reflect.Slice {
			lines := make([]string, value.Len())
			for j := 0; j < value.Len(); j++ {
				if value.Index(j).Kind() == reflect.String {
					lines[j] = value.Index(j).String()
				} else if value.Index(j).Kind() == reflect.Struct {
					a, b := getTableValuesFromInterface(value.Index(j).Interface())
					t = append(t, title.Name)
					v = append(v, []string{"================================"})
					t = append(t, a...)
					v = append(v, b...)
				}
			}
			t = append(t, title.Name)
			v = append(v, lines)
		} else if time, ok := value.Interface().(time.Time); ok {
			t = append(t, title.Name)
			v = append(v, []string{time.Format("2006-01-02 15:04:05")})
		} else if number, ok := value.Interface().(int); ok {
			t = append(t, title.Name)
			v = append(v, []string{strconv.Itoa(number)})
		} else if b, ok := value.Interface().(bool); ok {
			t = append(t, title.Name)
			v = append(v, []string{strconv.FormatBool(b)})
		} else if s, ok := value.Interface().(string); ok {
			lineArray := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
			t = append(t, title.Name)
			v = append(v, lineArray)
		} else {
			if !strings.Contains(strings.ToLower(title.Name), "winner") {
				t = append(t, title.Name)
				v = append(v, []string{value.String()})
			}
		}
	}
	return t, v
}

//TODO add channel to abort
//takes title and values slices and draws them as table
func fillTableFromSlices(titles []string, values [][]string, abort chan bool) {
	select {
	case <-abort:
		//aborts previous call
	default:
		//so it doesn't lock
	}
	aborted := make(chan bool)
	go func() {
		//waits for abort signal
		abort <- true
		aborted <- true
	}()
	infoTable.Clear()
	rowIndex := 0
	for index, title := range titles {
		//convert supported API IDs to reasonable strings
		lines := convertIDs(values[index])
		select {
		case <-aborted:
			return
		default:
			if len(values[index]) > 0 && len(values[index][0]) > 1 {
				//print title
				infoTable.SetCell(rowIndex, 1, tview.NewTableCell(title).SetAlign(tview.AlignRight).SetTextColor(tcell.ColorBlue))
				//print values
				for _, line := range lines {
					infoTable.SetCell(rowIndex, 2, tview.NewTableCell(line))
					rowIndex++
				}
				rowIndex++
			}
		}
	}
	infoTable.ScrollToBeginning()
	app.Draw()
}

//takes year/race ID and returns full year and race nuber as strings
func getYearAndRace(input string) (string, string, error) {
	var fullYear string
	var raceNumber string
	if len(input) < 4 {
		return fullYear, raceNumber, errors.New("not long enough")
	}
	_, err := strconv.Atoi(input[:4])
	if err != nil {
		return fullYear, raceNumber, errors.New("not a valid RearRaceID")
	}
	//TODO fix before 2020
	if input[:4] == "2018" || input[:4] == "2019" {
		return input[:4], "0", nil
	}
	year := input[:2]
	intYear, _ := strconv.Atoi(year)
	//TODO: change before 2030
	if intYear < 30 {
		fullYear = "20" + year
	} else {
		fullYear = "19" + year
	}
	raceNumber = input[2:4]
	return fullYear, raceNumber, nil
}

//prints to debug window
func debugPrint(s string, x ...string) {
	y := s
	for _, str := range x {
		y += " " + str
	}
	if debugText != nil {
		fmt.Fprintf(debugText, y+"\n")
		debugText.ScrollToEnd()
	}
}

func checkArgs(searchArg string) bool {
	for _, arg := range os.Args {
		if arg == searchArg {
			return true
		}
	}
	return false
}

func monitorCommand(node *tview.TreeNode, watchphrase string, output io.ReadCloser) {
	scanner := bufio.NewScanner(output)
	done := false
	go func() {
		for scanner.Scan() {
			sText := scanner.Text()
			debugPrint(sText)
			if strings.Contains(sText, watchphrase) {
				break
			}
		}
		done = true
	}()
	blinkNode(node, &done, tcell.ColorWhite)
	app.Draw()
}

func switchNode(node *tview.TreeNode) {
	reference := node.GetReference()
	if index, ok := reference.(int); ok && index < len(vodTypes.Objects) {
		v, t := getTableValuesFromInterface(vodTypes.Objects[index])
		go fillTableFromSlices(v, t, abortWritingInfo)
	} else if x := reflect.ValueOf(reference); x.Kind() == reflect.Struct {
		v, t := getTableValuesFromInterface(reference)
		go fillTableFromSlices(v, t, abortWritingInfo)
	} else if len(node.GetChildren()) != 0 {
		infoTable.Clear()
	}
	infoTable.ScrollToBeginning()
}

func nodeSelected(node *tview.TreeNode) {
	reference := node.GetReference()
	children := node.GetChildren()
	if reference == nil || node.GetText() == "loading..." {
		//Selecting the root node or a loading node does nothing
		return
	} else if len(children) > 0 {
		//Collapse if visible, expand if collapsed.
		node.SetExpanded(!node.IsExpanded())
	} else if ep, ok := reference.(episodeStruct); ok {
		//if regular episode is selected for the first time
		nodes := getPlaybackNodes(ep.Title, ep.Items[0])
		appendNodes(node, nodes...)
	} else if ep, ok := reference.(channelUrlsStruct); ok {
		//if single perspective is selected (main feed, driver onboards, etc.) from full race weekends
		//TODO: better name
		nodes := getPlaybackNodes(node.GetText(), ep.Self)
		appendNodes(node, nodes...)
	} else if event, ok := reference.(eventStruct); ok {
		//if event (eg. Australian GP 2018) is selected from full race weekends
		done := false
		hasSessions := false
		go func() {
			sessions := getSessionNodes(event)
			for _, session := range sessions {
				if session != nil && len(session.GetChildren()) > 0 {
					hasSessions = true
					node.AddChild(session)
				}
			}
			done = true
		}()
		go func() {
			blinkNode(node, &done, tcell.ColorWhite)
			if !hasSessions {
				node.SetColor(tcell.ColorRed)
				node.SetText(node.GetText() + " - NO CONTENT AVAILABLE")
				node.SetSelectable(false)
			}
			app.Draw()
		}()
	} else if season, ok := reference.(seasonStruct); ok {
		//if full season is selected from full race weekends
		done := false
		go func() {
			events := getEventNodes(season)
			for _, event := range events {
				layout := "2006-01-02"
				e := event.GetReference().(eventStruct)
				t, _ := time.Parse(layout, e.StartDate)
				if t.Before(time.Now().AddDate(0, 0, 1)) {
					node.AddChild(event)
				}
			}
			done = true
		}()
		go blinkNode(node, &done, tcell.ColorWheat)
	} else if context, ok := reference.(nodeContext); ok {
		//custom command
		monitor := false
		com := context.CustomOptions
		if com.Watchphrase != "" && com.CommandToWatch >= 0 && com.CommandToWatch < len(com.Commands) {
			monitor = true
		}
		var stdoutIn io.ReadCloser
		url := getPlayableURL(context.EpID)
		var filepath string
		fileLoaded := false
		//run every command
		go func() {
			for j := range com.Commands {
				if len(com.Commands[j]) > 0 {
					tmpCommand := make([]string, len(com.Commands[j]))
					copy(tmpCommand, com.Commands[j])
					//replace $url and $file
					for x, s := range tmpCommand {
						tmpCommand[x] = s
						if strings.Contains(s, "$file") {
							if !fileLoaded {
								filepath, _ = downloadAsset(url, context.Title)
								fileLoaded = true
							}
							tmpCommand[x] = strings.Replace(tmpCommand[x], "$file", filepath, -1)
						}
						tmpCommand[x] = strings.Replace(tmpCommand[x], "$url", url, -1)
					}
					//run command
					debugPrint("starting:", tmpCommand...)
					cmd := exec.Command(tmpCommand[0], tmpCommand[1:]...)
					stdoutIn, _ = cmd.StdoutPipe()
					err := cmd.Start()
					if err != nil {
						debugPrint(err.Error())
					}
					if monitor && com.CommandToWatch == j {
						go monitorCommand(node, com.Watchphrase, stdoutIn)
					}
					//wait for exit code if commands should not be executed concurrently
					if !com.Concurrent {
						err := cmd.Wait()
						if err != nil {
							debugPrint(err.Error())
						}
					}
				}
			}
			if !monitor {
				node.SetColor(tcell.ColorBlue)
			}
		}()
	} else if i, ok := reference.(int); ok {
		//if episodes for category are not loaded yet
		if i < len(vodTypes.Objects) {
			go func() {
				doneLoading := false
				go blinkNode(node, &doneLoading, tcell.ColorYellow)
				episodes := getEpisodeNodes(vodTypes.Objects[i].ContentUrls)
				appendNodes(node, episodes...)
				doneLoading = true
			}()
		}
	} else if _, ok := reference.(allSeasonStruct); ok {
		done := false
		go func() {
			seasons := getSeasonNodes()
			appendNodes(node, seasons...)
			node.SetReference(seasons)
			done = true
		}()
		go blinkNode(node, &done, tcell.ColorYellow)
	} else if node.GetText() == "Play with MPV" {
		cmd := exec.Command("mpv", getPlayableURL(reference.(string)), "--alang="+con.Lang, "--start=0")
		stdoutIn, _ := cmd.StdoutPipe()
		err := cmd.Start()
		if err != nil {
			debugPrint(err.Error())
		}
		go monitorCommand(node, "Video", stdoutIn)
	} else if node.GetText() == "Download .m3u8" {
		node.SetColor(tcell.ColorBlue)
		urlAndTitle := reference.([]string)
		downloadAsset(getPlayableURL(urlAndTitle[0]), urlAndTitle[1])
	} else if node.GetText() == "GET URL" {
		debugPrint(getPlayableURL(reference.(string)))
	} else if node.GetText() == "download update" {
		err := openbrowser("https://github.com/SoMuchForSubtlety/F1viewer/releases/latest")
		if err != nil {
			debugPrint(err.Error())
		}
	} else if node.GetText() == "don't tell me about updates" {
		debugPrint("meme")
		con.CheckUpdate = false
		err := con.save()
		if err != nil {
			debugPrint(err.Error())
		}
		node.SetColor(tcell.ColorBlue)
		node.SetText("update notifications turned off")
	}
}

func (cfg *config) save() error {

	d, err := json.MarshalIndent(&cfg, "", "\t")
	if err != nil {
		return fmt.Errorf("error marshaling config: %v", err)
	}

	err = ioutil.WriteFile("config.json", d, 0600)
	if err != nil {
		return fmt.Errorf("error saving config: %v", err)
	}
	return nil
}
