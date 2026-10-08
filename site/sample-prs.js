/* Sample pull requests for the triangulator panel (also used by the tests and `triangulate.py --sample`). */
window.SAMPLE_PRS = [
 {
  "number": 101,
  "title": "Fix use-after-free in AIGroup::removeAll",
  "body": "AIGroup::removeAll frees the group while iterating; crash when units die during removal.",
  "files": [
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/AIGroup.cpp",
    "patch": "@@ -410,6 +410,9 @@ void AIGroup::removeAll()\n   for (auto it = m_list.begin(); it != m_list.end(); )\n+    // copy before erase to avoid use after free\n+    AIObject *obj = *it;\n+    it = m_list.erase(it);\n-    delete *it;\n"
   }
  ]
 },
 {
  "number": 102,
  "title": "Prevent AIGroup removeAll use after free crash",
  "body": "Crash when units die while removeAll is iterating the group list. Erase before delete.",
  "files": [
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/AIGroup.cpp",
    "patch": "@@ -412,5 +412,8 @@ void AIGroup::removeAll()\n+    AIObject *obj = *it;\n+    it = m_list.erase(it); // erase before delete, use after free\n"
   }
  ]
 },
 {
  "number": 103,
  "title": "Rework pathfinding node allocation",
  "body": "Allocate pathfinder cells from a pool instead of new/delete.",
  "files": [
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/AIGroup.cpp",
    "patch": "@@ -405,10 +405,14 @@ void AIGroup::removeAll()\n+    PathNode *node = s_nodePool.acquire();\n+    s_nodePool.release(node);\n"
   },
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/Pathfinder.cpp",
    "patch": "@@ -50,3 +50,6 @@\n+  s_nodePool.init(4096);\n"
   }
  ]
 },
 {
  "number": 104,
  "title": "Add retail compatibility switch for networking",
  "body": "Guard the new packet format behind RETAIL_COMPATIBLE_NETWORKING.",
  "files": [
   {
    "path": "Core/GameEngine/Include/Common/GameDefines.h",
    "patch": "@@ -129,3 +129,6 @@\n+#define RETAIL_COMPATIBLE_NETWORKING (1)\n"
   },
   {
    "path": "Core/GameEngine/Source/GameNetwork/Connection.cpp",
    "patch": "@@ -88,4 +88,8 @@\n+  if (RETAIL_COMPATIBLE_NETWORKING) sendLegacyPacket();\n"
   }
  ]
 },
 {
  "number": 105,
  "title": "Add logging to AIGroup member changes",
  "body": "Log when a unit joins or leaves an AI group.",
  "files": [
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/AIGroupLog.cpp",
    "patch": "@@ -1,1 +1,20 @@\n+void logGroupChange(AIGroup *group) { DEBUG_LOG((\"AIGroup member changed\")); }\n"
   }
  ]
 },
 {
  "number": 106,
  "title": "Fix typo in README",
  "body": "Spelling.",
  "files": [
   {
    "path": "README.md",
    "patch": "@@ -3,1 +3,1 @@\n-teh\n+the\n"
   }
  ]
 },
 {
  "number": 107,
  "title": "Fix AIGroup::removeAll crash when the group is already empty",
  "body": "removeAll dereferences the list head after the group was emptied; guard against an empty group crash.",
  "files": [
   {
    "path": "GeneralsMD/Code/GameEngine/Source/GameLogic/AI/AIGroup.cpp",
    "patch": "@@ -900,4 +900,7 @@ void AIGroup::removeAll()\n+  if (m_list.empty()) return; // empty group guard\n"
   }
  ]
 }
];
